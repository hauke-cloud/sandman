// Package crd carries sandman's CustomResourceDefinition and installs it.
//
// The definition is embedded in the binary and applied at start-up rather than
// shipped in the Helm chart. A chart's `crds/` directory is installed once and
// never upgraded -- Helm deliberately refuses to touch a CRD on `helm upgrade`,
// because it cannot know what would be lost -- so a chart-installed CRD drifts
// behind the controller that depends on it, and the failure shows up as a field
// silently dropped by the API server. Applying it here binds the schema to the
// binary that understands it: the version of the CRD in the cluster is always
// the version this sandman was built with.
package crd

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldOwner is the field manager sandman applies its CRDs as. Server-side
// apply tracks ownership per field under this name, so a field an operator
// edits by hand is not silently reverted on the next start -- it conflicts,
// loudly, unless ownership is forced.
const FieldOwner = client.FieldOwner("sandman")

//go:embed assets/*.yaml
var assets embed.FS

// establishedTimeout bounds the wait for the API server to serve the new type.
// Establishing a CRD is fast; a minute is generous enough that hitting it means
// something is wrong rather than slow.
const establishedTimeout = time.Minute

// Definitions returns the embedded CustomResourceDefinitions as unstructured
// objects, in the order they are applied.
func Definitions() ([]*unstructured.Unstructured, error) {
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		return nil, fmt.Errorf("read embedded crds: %w", err)
	}

	definitions := make([]*unstructured.Unstructured, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := assets.ReadFile(path.Join("assets", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		object := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &object.Object); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if object.GetKind() != "CustomResourceDefinition" {
			return nil, fmt.Errorf("%s is a %s, not a CustomResourceDefinition", entry.Name(), object.GetKind())
		}
		definitions = append(definitions, object)
	}
	if len(definitions) == 0 {
		return nil, errors.New("no CustomResourceDefinitions are embedded in this binary")
	}
	return definitions, nil
}

// Install applies every embedded definition and waits for the API server to
// establish it. It must finish before the manager's cache starts, because an
// informer for a type the API server does not serve yet fails rather than
// waits.
func Install(ctx context.Context, c client.Client, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	definitions, err := Definitions()
	if err != nil {
		return err
	}

	for _, definition := range definitions {
		name := definition.GetName()
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(definition),
			FieldOwner, client.ForceOwnership); err != nil {
			if apierrors.IsForbidden(err) {
				// Worth naming precisely: a cluster that has the CRD installed
				// out of band and denies sandman the permission is a valid
				// setup, and the operator needs to know which of the two to
				// fix.
				return fmt.Errorf("applying CustomResourceDefinition %s was denied "+
					"(grant apiextensions.k8s.io/customresourcedefinitions, or install the CRD "+
					"out of band and start with -install-crds=false): %w", name, err)
			}
			return fmt.Errorf("apply CustomResourceDefinition %s: %w", name, err)
		}
		logger.Info("applied CustomResourceDefinition", "name", name)

		if err := WaitEstablished(ctx, c, name); err != nil {
			return err
		}
	}
	return nil
}

// WaitEstablished blocks until the named CRD is being served.
func WaitEstablished(ctx context.Context, c client.Reader, name string) error {
	ctx, cancel := context.WithTimeout(ctx, establishedTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true,
		func(ctx context.Context) (bool, error) {
			var definition apiextensionsv1.CustomResourceDefinition
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &definition); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			for _, condition := range definition.Status.Conditions {
				switch condition.Type {
				case apiextensionsv1.Established:
					if condition.Status == apiextensionsv1.ConditionTrue {
						return true, nil
					}
				case apiextensionsv1.NamesAccepted:
					if condition.Status == apiextensionsv1.ConditionFalse {
						// A name collision never resolves itself, so failing
						// here beats timing out sixty seconds later.
						return false, fmt.Errorf("CustomResourceDefinition %s was rejected: %s",
							name, condition.Message)
					}
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for CustomResourceDefinition %s to be established: %w", name, err)
	}
	return nil
}
