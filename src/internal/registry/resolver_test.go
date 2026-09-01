package registry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// selfSignedPEM generates a CA certificate at test time, so the fixture cannot
// expire and rot the suite.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pg-k8s-proxy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newResolver(t *testing.T, enabled bool, data map[string][]byte) *ClientResolver {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the built-in types: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-ca",
			Namespace: "apps",
			Labels:    map[string]string{CABundleLabel: "true"},
		},
		Data: data,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	return NewClientResolver(c).WithCABundles(enabled)
}

// Making the attempt is itself the hazard: a cache read starts the Secret
// informer and waits for it to sync, which never happens without the RBAC, so
// the call blocks on every rebuild for every route that asks for a bundle.
func TestLoadCABundleRefusesWhenDisabled(t *testing.T) {
	r := newResolver(t, false, map[string][]byte{"ca.crt": selfSignedPEM(t)})

	_, err := r.LoadCABundle(context.Background(), "apps", "backend-ca", "ca.crt")
	if err == nil {
		t.Fatal("reading a CA bundle succeeded while the feature is disabled")
	}
	if !strings.Contains(err.Error(), "rbac.readCABundleSecrets=true") {
		t.Errorf("the error does not say how to enable it: %v", err)
	}
}

func TestLoadCABundleReadsWhenEnabled(t *testing.T) {
	r := newResolver(t, true, map[string][]byte{"ca.crt": selfSignedPEM(t)})

	pool, err := r.LoadCABundle(context.Background(), "apps", "backend-ca", "ca.crt")
	if err != nil {
		t.Fatalf("LoadCABundle: %v", err)
	}
	if pool == nil {
		t.Fatal("no certificate pool was returned")
	}
}

func TestLoadCABundleReportsAMissingKey(t *testing.T) {
	r := newResolver(t, true, map[string][]byte{"ca.crt": selfSignedPEM(t)})

	_, err := r.LoadCABundle(context.Background(), "apps", "backend-ca", "absent.crt")
	if err == nil || !strings.Contains(err.Error(), "absent.crt") {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
}

func TestLoadCABundleRejectsNonPEMData(t *testing.T) {
	r := newResolver(t, true, map[string][]byte{"ca.crt": []byte("not a certificate")})

	_, err := r.LoadCABundle(context.Background(), "apps", "backend-ca", "ca.crt")
	if err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Errorf("error = %v, want it to report the missing PEM data", err)
	}
}

// The label is what keeps the informer's memory footprint and the granted RBAC
// narrow, so the error has to point at it when a Secret is not visible.
func TestLoadCABundleErrorMentionsTheRequiredLabel(t *testing.T) {
	r := newResolver(t, true, map[string][]byte{"ca.crt": selfSignedPEM(t)})

	_, err := r.LoadCABundle(context.Background(), "apps", "absent-secret", "ca.crt")
	if err == nil || !strings.Contains(err.Error(), CABundleLabel) {
		t.Errorf("error = %v, want it to mention %s", err, CABundleLabel)
	}
}
