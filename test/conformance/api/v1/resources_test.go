// +build e2e

/*
Copyright 2019 The Knative Authors

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

package v1

import (
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	pkgTest "knative.dev/pkg/test"
	"knative.dev/pkg/test/spoof"
	"knative.dev/serving/test"
	v1test "knative.dev/serving/test/v1"

	rtesting "knative.dev/serving/pkg/testing/v1"
)

func TestCustomResourcesLimits(t *testing.T) {
	t.Parallel()
	clients := test.Setup(t)

	t.Log("Creating a new Route and Configuration")
	withResources := rtesting.WithResourceRequirements(corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("350Mi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("350Mi"),
		},
	})

	names := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   test.Autoscale,
	}

	test.CleanupOnInterrupt(func() { test.TearDown(clients, names) })
	defer test.TearDown(clients, names)

	objects, err := v1test.CreateServiceReady(t, clients, &names, withResources)
	if err != nil {
		t.Fatalf("Failed to create initial Service %v: %v", names.Service, err)
	}
	domain := objects.Route.Status.URL.Host

	_, err = pkgTest.WaitForEndpointState(
		clients.KubeClient,
		t.Logf,
		domain,
		v1test.RetryingRouteInconsistency(pkgTest.MatchesAllOf(pkgTest.IsStatusOK)),
		"ResourceTestServesText",
		test.ServingFlags.ResolvableDomain)
	if err != nil {
		t.Fatalf("Error probing domain %s: %v", domain, err)
	}

	sendPostRequest := func(resolvableDomain bool, domain string, query string) (*spoof.Response, error) {
		t.Logf("The domain of request is %s and its query is %s", domain, query)
		client, err := pkgTest.NewSpoofingClient(clients.KubeClient, t.Logf, domain, resolvableDomain)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s?%s", domain, query), nil)
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}

	pokeCowForMB := func(mb int) error {
		response, err := sendPostRequest(test.ServingFlags.ResolvableDomain, domain, fmt.Sprintf("bloat=%d", mb))
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("StatusCode = %d, want %d", response.StatusCode, http.StatusOK)
		}
		return nil
	}

	t.Log("Querying the application to see if the memory limits are enforced.")
	if err := pokeCowForMB(100); err != nil {
		t.Fatalf("Didn't get a response from bloating cow with %d MBs of Memory: %v", 100, err)
	}

	if err := pokeCowForMB(200); err != nil {
		t.Fatalf("Didn't get a response from bloating cow with %d MBs of Memory: %v", 200, err)
	}

	if err := pokeCowForMB(500); err == nil {
		t.Fatalf("We shouldn't have got a response from bloating cow with %d MBs of Memory: %v", 500, err)
	}
}
