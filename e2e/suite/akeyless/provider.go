/*
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

package akeyless

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	//nolint
	. "github.com/onsi/ginkgo"

	//nolint
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	esv1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
	"github.com/external-secrets/external-secrets/e2e/framework"

	aws_cloud_id "github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/aws"
	azure_cloud_id "github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/azure"
	gcp_cloud_id "github.com/akeylesslabs/akeyless-go-cloud-id/cloudprovider/gcp"
	"github.com/akeylesslabs/akeyless-go/v2"
)

type akeylessProvider struct {
	accessID        string
	accessType      string
	accessTypeParam string
	framework       *framework.Framework
	restApiClient   *akeyless.V2ApiService
}

var apiErr akeyless.GenericOpenAPIError

const DefServiceAccountFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

func newAkeylessProvider(f *framework.Framework, accessID, accessType, accessTypeParam string) *akeylessProvider {
	prov := &akeylessProvider{
		accessID:        accessID,
		accessType:      accessType,
		accessTypeParam: accessTypeParam,
		framework:       f,
	}

	restApiClient := akeyless.NewAPIClient(&akeyless.Configuration{
		Servers: []akeyless.ServerConfiguration{
			{
				URL: "https://api.akeyless.io",
			},
		},
	}).V2Api

	prov.restApiClient = restApiClient

	BeforeEach(prov.BeforeEach)
	return prov
}

// CreateSecret creates a secret.
func (a *akeylessProvider) CreateSecret(key, val string) {
	token, err := a.GetToken()
	Expect(err).ToNot(HaveOccurred())

	ctx := context.Background()
	gsvBody := akeyless.CreateSecret{
		Name:  key,
		Value: val,
		Token: &token,
	}

	_, _, err = a.restApiClient.CreateSecret(ctx).Body(gsvBody).Execute()
	Expect(err).ToNot(HaveOccurred())
}

func (a *akeylessProvider) DeleteSecret(key string) {
	token, err := a.GetToken()
	Expect(err).ToNot(HaveOccurred())

	ctx := context.Background()
	gsvBody := akeyless.DeleteItem{
		Name:  key,
		Token: &token,
	}

	_, _, err = a.restApiClient.DeleteItem(ctx).Body(gsvBody).Execute()
	Expect(err).ToNot(HaveOccurred())
}

func (a *akeylessProvider) BeforeEach() {
	// Creating an Akeyless secret
	akeylessCreds := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider-secret",
			Namespace: a.framework.Namespace.Name,
		},
		StringData: map[string]string{
			"access-id":         a.accessID,
			"access-type":       a.accessType,
			"access-type-param": a.accessTypeParam,
		},
	}
	err := a.framework.CRClient.Create(context.Background(), akeylessCreds)
	Expect(err).ToNot(HaveOccurred())

	// Creating Akeyless secret store
	secretStore := &esv1alpha1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      a.framework.Namespace.Name,
			Namespace: a.framework.Namespace.Name,
		},
		Spec: esv1alpha1.SecretStoreSpec{
			Provider: &esv1alpha1.SecretStoreProvider{
				Akeyless: &esv1alpha1.AkeylessProvider{
					Auth: &esv1alpha1.AkeylessAuth{
						SecretRef: esv1alpha1.AkeylessAuthSecretRef{
							AccessID: esmeta.SecretKeySelector{
								Name: "access-id-secret",
								Key:  "access-id",
							},
							AccessType: esmeta.SecretKeySelector{
								Name: "access-type-secret",
								Key:  "access-type",
							},
							AccessTypeParam: esmeta.SecretKeySelector{
								Name: "access-type-param-secert",
								Key:  "access-type-param",
							},
						},
					},
				},
			},
		},
	}
	err = a.framework.CRClient.Create(context.Background(), secretStore)
	Expect(err).ToNot(HaveOccurred())
}

func (a *akeylessProvider) GetToken() (string, error) {

	ctx := context.Background()
	authBody := akeyless.NewAuthWithDefaults()
	authBody.AccessId = akeyless.PtrString(a.accessID)

	if a.accessType == "api_key" {
		authBody.AccessKey = akeyless.PtrString(a.accessTypeParam)
	} else if a.accessType == "k8s" {
		jwtString, err := readK8SServiceAccountJWT()
		if err != nil {
			return "", fmt.Errorf("failed to read JWT with Kubernetes Auth from %v. error: %v", DefServiceAccountFile, err.Error())
		}
		K8SAuthConfigName := a.accessTypeParam
		authBody.AccessType = akeyless.PtrString(a.accessType)
		authBody.K8sServiceAccountToken = akeyless.PtrString(jwtString)
		authBody.K8sAuthConfigName = akeyless.PtrString(K8SAuthConfigName)
	} else {
		cloudId, err := a.getCloudId(a.accessType, a.accessTypeParam)
		if err != nil {
			return "", fmt.Errorf("Require Cloud ID " + err.Error())
		}
		authBody.AccessType = akeyless.PtrString(a.accessType)
		authBody.CloudId = akeyless.PtrString(cloudId)
	}

	authOut, _, err := a.restApiClient.Auth(ctx).Body(*authBody).Execute()
	if err != nil {
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("authentication failed: %v", string(apiErr.Body()))
		}
		return "", fmt.Errorf("authentication failed: %v", err)
	}

	token := authOut.GetToken()
	return token, nil
}

func (a *akeylessProvider) getCloudId(provider string, accTypeParam string) (string, error) {
	var cloudId string
	var err error

	switch provider {
	case "azure_ad":
		cloudId, err = azure_cloud_id.GetCloudId(accTypeParam)
	case "aws_iam":
		cloudId, err = aws_cloud_id.GetCloudId()
	case "gcp":
		cloudId, err = gcp_cloud_id.GetCloudID(accTypeParam)
	default:
		return "", fmt.Errorf("Unable to determine provider: %s", provider)
	}
	return cloudId, err
}

// readK8SServiceAccountJWT reads the JWT data for the Agent to submit to Akeyless Gateway.
func readK8SServiceAccountJWT() (string, error) {
	data, err := os.Open(DefServiceAccountFile)
	if err != nil {
		return "", err
	}
	defer data.Close()

	contentBytes, err := ioutil.ReadAll(data)
	if err != nil {
		return "", err
	}

	a := strings.TrimSpace(string(contentBytes))

	return base64.StdEncoding.EncodeToString([]byte(a)), nil
	//return encoding_ex.Base64Encode([]byte(a)), nil
}
