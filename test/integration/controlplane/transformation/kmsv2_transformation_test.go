//go:build !windows
// +build !windows

/*
Copyright 2022 The Kubernetes Authors.

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

package transformation

import (
	"bytes"
	"context"
	"crypto/aes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/options/encryptionconfig"
	"k8s.io/apiserver/pkg/storage/value"
	aestransformer "k8s.io/apiserver/pkg/storage/value/encrypt/aes"
	"k8s.io/apiserver/pkg/storage/value/encrypt/envelope/kmsv2"
	kmstypes "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/kmsv2/v2alpha1"
	kmsv2mock "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/testing/v2alpha1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kmsv2api "k8s.io/kms/apis/v2alpha1"
	"k8s.io/kubernetes/test/integration/etcd"
)

type envelopekmsv2 struct {
	providerName string
	rawEnvelope  []byte
	plainTextDEK []byte
}

func (r envelopekmsv2) prefix() string {
	return fmt.Sprintf("k8s:enc:kms:v2:%s:", r.providerName)
}

func (r envelopekmsv2) prefixLen() int {
	return len(r.prefix())
}

func (r envelopekmsv2) cipherTextDEK() ([]byte, error) {
	o := &kmstypes.EncryptedObject{}
	if err := proto.Unmarshal(r.rawEnvelope[r.startOfPayload(r.providerName):], o); err != nil {
		return nil, err
	}
	return o.EncryptedDEK, nil
}

func (r envelopekmsv2) startOfPayload(_ string) int {
	return r.prefixLen()
}

func (r envelopekmsv2) cipherTextPayload() ([]byte, error) {
	o := &kmstypes.EncryptedObject{}
	if err := proto.Unmarshal(r.rawEnvelope[r.startOfPayload(r.providerName):], o); err != nil {
		return nil, err
	}
	return o.EncryptedData, nil
}

func (r envelopekmsv2) plainTextPayload(secretETCDPath string) ([]byte, error) {
	block, err := aes.NewCipher(r.plainTextDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AES Cipher: %v", err)
	}
	ctx := context.Background()
	dataCtx := value.DefaultContext([]byte(secretETCDPath))
	aesgcmTransformer := aestransformer.NewGCMTransformer(block)
	data, err := r.cipherTextPayload()
	if err != nil {
		return nil, fmt.Errorf("failed to get cipher text payload: %v", err)
	}
	plainSecret, _, err := aesgcmTransformer.TransformFromStorage(ctx, data, dataCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to transform from storage via AESGCM, err: %w", err)
	}

	return plainSecret, nil
}

// TestKMSv2Provider is an integration test between KubeAPI, ETCD and KMSv2 Plugin
// Concretely, this test verifies the following integration contracts:
// 1. Raw records in ETCD that were processed by KMSv2 Provider should be prefixed with k8s:enc:kms:v2:<plugin name>:
// 2. Data Encryption Key (DEK) should be generated by envelopeTransformer and passed to KMS gRPC Plugin
// 3. KMS gRPC Plugin should encrypt the DEK with a Key Encryption Key (KEK) and pass it back to envelopeTransformer
// 4. The cipherTextPayload (ex. Secret) should be encrypted via AES GCM transform
// 5. kmstypes.EncryptedObject structure should be serialized and deposited in ETCD
func TestKMSv2Provider(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`

	providerName := "kms-provider"
	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer test.cleanUp()

	test.secret, err = test.createSecret(testSecret, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test secret, error: %v", err)
	}

	// Since Data Encryption Key (DEK) is randomly generated (per encryption operation), we need to ask KMS Mock for it.
	plainTextDEK := pluginMock.LastEncryptRequest()

	secretETCDPath := test.getETCDPath()
	rawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}

	envelopeData := envelopekmsv2{
		providerName: providerName,
		rawEnvelope:  rawEnvelope,
		plainTextDEK: plainTextDEK,
	}

	wantPrefix := string(envelopeData.prefix())
	if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefix)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ciphertext, err := envelopeData.cipherTextDEK()
	if err != nil {
		t.Fatalf("failed to get ciphertext DEK from KMSv2 Plugin: %v", err)
	}
	decryptResponse, err := pluginMock.Decrypt(ctx, &kmsv2api.DecryptRequest{Uid: string(types.UID(uuid.NewUUID())), Ciphertext: ciphertext})
	if err != nil {
		t.Fatalf("failed to decrypt DEK, %v", err)
	}
	dekPlainAsWouldBeSeenByETCD := decryptResponse.Plaintext

	if !bytes.Equal(plainTextDEK, dekPlainAsWouldBeSeenByETCD) {
		t.Fatalf("expected plainTextDEK %v to be passed to KMS Plugin, but got %s",
			plainTextDEK, dekPlainAsWouldBeSeenByETCD)
	}

	plainSecret, err := envelopeData.plainTextPayload(secretETCDPath)
	if err != nil {
		t.Fatalf("failed to transform from storage via AESGCM, err: %v", err)
	}

	if !strings.Contains(string(plainSecret), secretVal) {
		t.Fatalf("expected %q after decryption, but got %q", secretVal, string(plainSecret))
	}

	secretClient := test.restClient.CoreV1().Secrets(testNamespace)
	// Secrets should be un-enveloped on direct reads from Kube API Server.
	s, err := secretClient.Get(ctx, testSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Secret from %s, err: %v", testNamespace, err)
	}
	if secretVal != string(s.Data[secretKey]) {
		t.Fatalf("expected %s from KubeAPI, but got %s", secretVal, string(s.Data[secretKey]))
	}
}

func TestKMSv2Healthz(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       apiVersion: v2
       name: provider-1
       endpoint: unix:///@kms-provider-1.sock
    - kms:
       apiVersion: v2
       name: provider-2
       endpoint: unix:///@kms-provider-2.sock
`

	pluginMock1, err := kmsv2mock.NewBase64Plugin("@kms-provider-1.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin #1: %v", err)
	}

	if err := pluginMock1.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock1.CleanUp()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock1); err != nil {
		t.Fatalf("Failed to start plugin #1, err: %v", err)
	}

	pluginMock2, err := kmsv2mock.NewBase64Plugin("@kms-provider-2.sock")
	if err != nil {
		t.Fatalf("Failed to create mock of KMS Plugin #2: err: %v", err)
	}
	if err := pluginMock2.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock2.CleanUp()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock2); err != nil {
		t.Fatalf("Failed to start KMS Plugin #2: err: %v", err)
	}

	test, err := newTransformTest(t, encryptionConfig)
	if err != nil {
		t.Fatalf("Failed to start kube-apiserver, error: %v", err)
	}
	defer test.cleanUp()

	// Name of the healthz check is calculated based on a constant "kms-provider-" + position of the
	// provider in the config.

	// Stage 1 - Since all kms-plugins are guaranteed to be up, healthz checks for:
	// healthz/kms-provider-0 and /healthz/kms-provider-1 should be OK.
	mustBeHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)

	// Stage 2 - kms-plugin for provider-1 is down. Therefore, expect the health check for provider-1
	// to fail, but provider-2 should still be OK
	pluginMock1.EnterFailedState()
	mustBeUnHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)
	pluginMock1.ExitFailedState()

	// Stage 3 - kms-plugin for provider-1 is now up. Therefore, expect the health check for provider-1
	// to succeed now, but provider-2 is now down.
	// Need to sleep since health check chases responses for 3 seconds.
	pluginMock2.EnterFailedState()
	mustBeHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeUnHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)
}

func TestKMSv2SingleService(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	var kmsv2Calls int
	origEnvelopeKMSv2ServiceFactory := encryptionconfig.EnvelopeKMSv2ServiceFactory
	encryptionconfig.EnvelopeKMSv2ServiceFactory = func(ctx context.Context, endpoint string, callTimeout time.Duration) (kmsv2.Service, error) {
		kmsv2Calls++
		return origEnvelopeKMSv2ServiceFactory(ctx, endpoint, callTimeout)
	}
	t.Cleanup(func() {
		encryptionconfig.EnvelopeKMSv2ServiceFactory = origEnvelopeKMSv2ServiceFactory
	})

	// check resources provided by the three servers that we have wired together
	// - pods and config maps from KAS
	// - CRDs and CRs from API extensions
	// - API services from aggregator
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - pods
    - configmaps
    - customresourcedefinitions.apiextensions.k8s.io
    - pandas.awesome.bears.com
    - apiservices.apiregistration.k8s.io
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`

	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	t.Cleanup(pluginMock.CleanUp)

	test, err := newTransformTest(t, encryptionConfig)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	t.Cleanup(test.cleanUp)

	// the storage registry for CRs is dynamic so create one to exercise the wiring
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(test.kubeAPIServer.ClientConfig), false, etcd.GetCustomResourceDefinitionData()...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	gvr := schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}
	stub := etcd.GetEtcdStorageData()[gvr].Stub
	dynamicClient, obj, err := etcd.JSONToUnstructured(stub, "", &meta.RESTMapping{
		Resource:         gvr,
		GroupVersionKind: gvr.GroupVersion().WithKind("Panda"),
		Scope:            meta.RESTScopeRoot,
	}, dynamic.NewForConfigOrDie(test.kubeAPIServer.ClientConfig))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dynamicClient.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if kmsv2Calls != 1 {
		t.Fatalf("expected a single call to KMS v2 service factory: %v", kmsv2Calls)
	}
}