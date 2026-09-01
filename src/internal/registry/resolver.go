package registry

import (
	"context"
	"crypto/x509"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CABundleLabel marks the Secrets the manager is allowed to read for backend CA
// bundles. Restricting the cache to labelled Secrets keeps both the informer's
// memory footprint and the granted RBAC narrow.
const CABundleLabel = "pgproxy.io/ca-bundle"

// ClientResolver resolves backend references against the shared cache.
type ClientResolver struct {
	Client client.Reader
}

// NewClientResolver returns a Resolver reading through c.
func NewClientResolver(c client.Reader) *ClientResolver { return &ClientResolver{Client: c} }

// ResolveServicePort maps a Service port reference onto a port number. A
// numeric reference is validated against the Service's declared ports so that a
// typo surfaces as a route condition rather than as connections that hang.
func (r *ClientResolver) ResolveServicePort(ctx context.Context, namespace, name string, port intstr.IntOrString) (int32, error) {
	var svc corev1.Service
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
		return 0, fmt.Errorf("service %s/%s: %w", namespace, name, err)
	}

	switch port.Type {
	case intstr.Int:
		want := port.IntVal
		for _, p := range svc.Spec.Ports {
			if p.Port == want {
				return want, nil
			}
		}
		return 0, fmt.Errorf("service %s/%s exposes no port %d", namespace, name, want)

	case intstr.String:
		// An all-digit string is how the CRD default ("5432") and most
		// hand-written manifests arrive; treat it as a number.
		if numeric, err := parsePortNumber(port.StrVal); err == nil {
			for _, p := range svc.Spec.Ports {
				if p.Port == numeric {
					return numeric, nil
				}
			}
			return 0, fmt.Errorf("service %s/%s exposes no port %d", namespace, name, numeric)
		}
		for _, p := range svc.Spec.Ports {
			if p.Name == port.StrVal {
				return p.Port, nil
			}
		}
		return 0, fmt.Errorf("service %s/%s has no port named %q", namespace, name, port.StrVal)
	}

	return 0, fmt.Errorf("service %s/%s: unrecognised port reference", namespace, name)
}

// LoadCABundle reads and parses a PEM CA bundle from a Secret key.
func (r *ClientResolver) LoadCABundle(ctx context.Context, namespace, secretName, key string) (*x509.CertPool, error) {
	var secret corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &secret); err != nil {
		return nil, fmt.Errorf("secret %s/%s (it must carry the label %s=true to be readable): %w",
			namespace, secretName, CABundleLabel, err)
	}

	pem, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s has no key %q", namespace, secretName, key)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("secret %s/%s key %q holds no PEM certificates", namespace, secretName, key)
	}
	return pool, nil
}

// parsePortNumber reports whether s is a plain port number. The CRD's default
// ("5432") and most hand-written manifests arrive as an all-digit string, so
// this is what tells a numeric port apart from a named one. ParseUint rejects
// signs, so "+5432" and "-1" both fall through to the named-port lookup.
func parsePortNumber(s string) (int32, error) {
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%q is not a port number: %w", s, err)
	}
	if port == 0 {
		return 0, fmt.Errorf("port 0 is not a valid port")
	}
	return int32(port), nil
}
