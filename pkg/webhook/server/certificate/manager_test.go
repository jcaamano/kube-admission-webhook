package certificate

import (
	"bytes"
	"context"
	"crypto/x509"
	"io/ioutil"
	"os"
	"testing"
	"time"

	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var (
	log = logf.Log.WithName("webhook/server/certificate/manager_test")
)

func init() {

	klog.InitFlags(nil)
	logf.SetLogger(logf.ZapLogger(true))

}

func TestSetRotationDeadline(t *testing.T) {

	defer func(original func(float64) time.Duration) { jitteryDuration = original }(jitteryDuration)

	now := time.Now()
	testCases := []struct {
		name         string
		notBefore    time.Time
		notAfter     time.Time
		shouldRotate bool
	}{
		{"just issued, still good", now.Add(-1 * time.Hour), now.Add(99 * time.Hour), false},
		{"half way expired, still good", now.Add(-24 * time.Hour), now.Add(24 * time.Hour), false},
		{"mostly expired, still good", now.Add(-69 * time.Hour), now.Add(31 * time.Hour), false},
		{"just about expired, should rotate", now.Add(-91 * time.Hour), now.Add(9 * time.Hour), true},
		{"nearly expired, should rotate", now.Add(-99 * time.Hour), now.Add(1 * time.Hour), true},
		{"already expired, should rotate", now.Add(-10 * time.Hour), now.Add(-1 * time.Hour), true},
		{"long duration", now.Add(-6 * 30 * 24 * time.Hour), now.Add(6 * 30 * 24 * time.Hour), true},
		{"short duration", now.Add(-30 * time.Second), now.Add(30 * time.Second), true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := Manager{
				caCert: &x509.Certificate{
					NotBefore: tc.notBefore,
					NotAfter:  tc.notAfter,
				},
				now: func() time.Time { return now },
				log: log,
			}
			jitteryDuration = func(float64) time.Duration { return time.Duration(float64(tc.notAfter.Sub(tc.notBefore)) * 0.7) }
			lowerBound := tc.notBefore.Add(time.Duration(float64(tc.notAfter.Sub(tc.notBefore)) * 0.7))

			deadline := m.nextRotationDeadline()

			if !deadline.Equal(lowerBound) {
				t.Errorf("For notBefore %v, notAfter %v, the rotationDeadline %v should be %v.",
					tc.notBefore,
					tc.notAfter,
					deadline,
					lowerBound)
			}
		})
	}
}

func TestWaitForDeadlineAndRotate(t *testing.T) {

	certDir, err := ioutil.TempDir("/tmp/", "manager-test-certs")
	if err != nil {
		t.Errorf("failed creating temporal dir for certs %v", err)
	}
	defer os.RemoveAll(certDir)

	mutatingWebhookConfiguration := &admissionregistrationv1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fooWebhook",
		},
		Webhooks: []admissionregistrationv1beta1.MutatingWebhook{
			admissionregistrationv1beta1.MutatingWebhook{
				Name: "fooWebhook",
				ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
					Service: &admissionregistrationv1beta1.ServiceReference{
						Name:      "fooWebhook",
						Namespace: "fooWebhook",
					},
				},
			},
		},
	}

	objs := []runtime.Object{mutatingWebhookConfiguration}

	client := fake.NewFakeClient(objs...)
	certsDuration := time.Minute
	manager := NewManager(client, "fooWebhook", MutatingWebhook, certsDuration)
	manager.waitForDeadlineAndRotate()
	//TODO Implement ErrorsHandler to take the errors that we have at
	//     background

	err = client.Get(context.TODO(), types.NamespacedName{Name: "fooWebhook"}, mutatingWebhookConfiguration)
	if err != nil {
		t.Fatalf("get mutatingwebhookconfiguration: (%v)", err)
	}

	clientConfig := mutatingWebhookConfiguration.Webhooks[0].ClientConfig
	if len(clientConfig.CABundle) == 0 {
		t.Error("CA bundle not updated")
	}

	secret := corev1.Secret{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: "fooWebhook", Namespace: "fooWebhook"}, &secret)
	if err != nil {
		t.Fatalf("get secret: (%v)", err)
	}

	if secret.Type != corev1.SecretTypeTLS {
		t.Fatalf("Non TLS secret type %s", secret.Type)
	}

	if len(secret.Data) == 0 {
		t.Fatal("No tls key/cert at secret")
	}

	nextRotation := time.Now().Sub(manager.nextRotationDeadline())
	start := time.Now()
	err = wait.PollImmediate(nextRotation, nextRotation, func() (bool, error) {
		manager.waitForDeadlineAndRotate()
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed waitting for rotation: (%v)", err)
	}
	elapsed := time.Now().Sub(start)

	if elapsed < nextRotation {
		t.Fatalf("rotation done before deadline %s expected %s", elapsed, nextRotation)
	}

	err = client.Get(context.TODO(), types.NamespacedName{Name: "fooWebhook"}, mutatingWebhookConfiguration)
	if err != nil {
		t.Fatalf("get mutatingwebhookconfiguration: (%v)", err)
	}
	newClientConfig := mutatingWebhookConfiguration.Webhooks[0].ClientConfig
	if bytes.Compare(newClientConfig.CABundle, clientConfig.CABundle) == 0 {
		t.Fatal("CABundle not updated after rotation")
	}

	newSecret := corev1.Secret{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: "fooWebhook", Namespace: "fooWebhook"}, &newSecret)
	if err != nil {
		t.Fatalf("get secret: (%v)", err)
	}

	if bytes.Compare(newSecret.Data[corev1.TLSPrivateKeyKey], secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Fatal("Secret data not updated before expiration time")
	}

	if bytes.Compare(newSecret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSCertKey]) == 0 {
		t.Fatal("Secret data not updated before expiration time")
	}

}
