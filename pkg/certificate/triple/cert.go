/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package triple

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math"
	"math/big"
	"net"
	"time"

	"github.com/pkg/errors"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	rsaKeySize = 2048
)

var (
	Now = func() time.Time { return time.Now() }
)

// Config contains the basic fields required for creating a certificate
type Config struct {
	CommonName   string
	Organization []string
	AltNames     AltNames
	Usages       []x509.ExtKeyUsage
}

// AltNames contains the domain names and IP addresses that will be added
// to the API Server's x509 certificate SubAltNames field. The values will
// be passed directly to the x509.Certificate object.
type AltNames struct {
	DNSNames []string
	IPs      []net.IP
}

// NewPrivateKey creates an RSA private key
func NewPrivateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(cryptorand.Reader, rsaKeySize)
}

// NewSelfSignedCACert creates a CA certificate
func NewSelfSignedCACert(cfg Config, key crypto.Signer, duration time.Duration) (*x509.Certificate, error) {
	now := Now()
	tmpl := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(duration).UTC(),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDERBytes, err := x509.CreateCertificate(cryptorand.Reader, &tmpl, &tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(certDERBytes)
}

// NewSignedCert creates a signed certificate using the given CA certificate and key
func NewSignedCert(cfg Config, key crypto.Signer, caCert *x509.Certificate, caKey crypto.Signer, duration time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64))
	if err != nil {
		return nil, err
	}
	if len(cfg.CommonName) == 0 {
		return nil, errors.New("must specify a CommonName")
	}
	if len(cfg.Usages) == 0 {
		return nil, errors.New("must specify at least one ExtKeyUsage")
	}

	certTmpl := x509.Certificate{
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		DNSNames:     cfg.AltNames.DNSNames,
		IPAddresses:  cfg.AltNames.IPs,
		SerialNumber: serial,
		NotBefore:    caCert.NotBefore,
		NotAfter:     Now().Add(duration).UTC(),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  cfg.Usages,
	}

	certDERBytes, err := x509.CreateCertificate(cryptorand.Reader, &certTmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(certDERBytes)
}

// MakeEllipticPrivateKeyPEM creates an ECDSA private key
func MakeEllipticPrivateKeyPEM() ([]byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return nil, err
	}

	derBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	privateKeyPemBlock := &pem.Block{
		Type:  ECPrivateKeyBlockType,
		Bytes: derBytes,
	}
	return pem.EncodeToMemory(privateKeyPemBlock), nil
}

func ipsToStrings(ips []net.IP) []string {
	ss := make([]string, 0, len(ips))
	for _, ip := range ips {
		ss = append(ss, ip.String())
	}
	return ss
}

func VerifyTLS(certsPEM, keyPEM, caBundle []byte) error {
	logger := logf.Log.WithName("VerifyTLS")

	_, err := ParsePrivateKeyPEM(keyPEM)
	if err != nil {
		return errors.Wrap(err, "failed parsing PEM TLS key")
	}

	certs, err := ParseCertsPEM(certsPEM)
	if err != nil {
		return errors.Wrap(err, "failed parsing PEM TLS certs")
	}

	cas := x509.NewCertPool()
	ok := cas.AppendCertsFromPEM([]byte(caBundle))
	if !ok {
		return errors.New("failed to parse CA bundle")
	}

	opts := x509.VerifyOptions{
		Roots:       cas,
		DNSName:     certs[0].DNSNames[0],
		CurrentTime: Now(),
	}

	if _, err := certs[0].Verify(opts); err != nil {
		return errors.Wrap(err, "failed to verify certificate")
	}

	logger.Info("TLS certificates chain verified")
	return nil
}
