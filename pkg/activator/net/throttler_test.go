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

package activator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"knative.dev/pkg/controller"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/system"
	_ "knative.dev/pkg/system/testing"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/apis/serving/v1alpha1"
	servingfake "knative.dev/serving/pkg/client/clientset/versioned/fake"
	servinginformers "knative.dev/serving/pkg/client/informers/externalversions"
	"knative.dev/serving/pkg/queue"
)

const defaultMaxConcurrency = 1000

type tryResult struct {
	Dest      string
	ErrString string
}

func TestThrottlerWithError(t *testing.T) {
	defer ClearAll()
	for _, tc := range []struct {
		name        string
		revisions   []*v1alpha1.Revision
		initUpdates []RevisionDestsUpdate
		deletes     []types.NamespacedName
		trys        []types.NamespacedName
		wantResults []tryResult
	}{{
		name: "second request timeout",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:           types.NamespacedName{testNamespace, testRevision},
			ClusterIPDest: "129.0.0.1:1234",
			Dests:         sets.NewString("128.0.0.1:1234"),
		}},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
			{Namespace: testNamespace, Name: testRevision},
		},
		wantResults: []tryResult{
			{Dest: "129.0.0.1:1234"},
			{ErrString: context.DeadlineExceeded.Error()},
		},
	}, {
		name: "remove before try",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:   types.NamespacedName{testNamespace, testRevision},
			Dests: sets.NewString("128.0.0.1:1234"),
		}},
		deletes: []types.NamespacedName{
			{testNamespace, testRevision},
		},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
		},
		wantResults: []tryResult{
			{ErrString: "revision.serving.knative.dev \"test-revision\" not found"},
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			updateCh := make(chan RevisionDestsUpdate, 2)

			params := queue.BreakerParams{
				QueueDepth:      1,
				MaxConcurrency:  defaultMaxConcurrency,
				InitialCapacity: 0,
			}

			fake := kubefake.NewSimpleClientset()
			informer := kubeinformers.NewSharedInformerFactory(fake, 0)
			endpoints := informer.Core().V1().Endpoints()

			servfake := servingfake.NewSimpleClientset()
			servinginformer := servinginformers.NewSharedInformerFactory(servfake, 0)
			revisions := servinginformer.Serving().V1alpha1().Revisions()

			stopCh := make(chan struct{})
			defer close(stopCh)
			controller.StartInformers(stopCh, endpoints.Informer(), revisions.Informer())

			// Add the revision were testing
			for _, rev := range tc.revisions {
				servfake.ServingV1alpha1().Revisions(rev.Namespace).Create(rev)
				revisions.Informer().GetIndexer().Add(rev)
			}

			throttler := NewThrottler(params, revisions, endpoints, TestLogger(t))
			for _, update := range tc.initUpdates {
				updateCh <- update
			}
			close(updateCh)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				throttler.Run(updateCh)
			}()

			// Wait for throttler to complete processing updates and exit
			wg.Wait()

			for _, delRev := range tc.deletes {
				servfake.ServingV1alpha1().Revisions(delRev.Namespace).Delete(delRev.Name, nil)
				revisions.Informer().GetIndexer().Delete(delRev)
			}

			// Make sure our informer event has fired
			if len(tc.deletes) > 0 {
				time.Sleep(200 * time.Millisecond)
			}

			tryContext, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
			defer cancel()

			gotTries := tryThrottler(throttler, tc.trys, tryContext)

			if got, want := gotTries, tc.wantResults; !cmp.Equal(got, want) {
				t.Errorf("Dests = %v, want: %v, diff: %s", got, want, cmp.Diff(want, got))
			}
		})
	}
}

func TestThrottlerSuccesses(t *testing.T) {
	defer ClearAll()
	for _, tc := range []struct {
		name        string
		revisions   []*v1alpha1.Revision
		initUpdates []RevisionDestsUpdate
		deletes     []types.NamespacedName
		trys        []types.NamespacedName
		wantDests   sets.String
	}{{
		name: "single healthy podIP",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:   types.NamespacedName{testNamespace, testRevision},
			Dests: sets.NewString("128.0.0.1:1234"),
		}},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
		},
		wantDests: sets.NewString("128.0.0.1:1234"),
	}, {
		name: "single healthy clusterIP",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:   types.NamespacedName{testNamespace, testRevision},
			Dests: sets.NewString("128.0.0.1:1234", "128.0.0.2:1234"),
		}, {
			Rev:           types.NamespacedName{testNamespace, testRevision},
			ClusterIPDest: "129.0.0.1:1234",
			Dests:         sets.NewString("128.0.0.1:1234"),
		}},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
		},
		wantDests: sets.NewString("129.0.0.1:1234"),
	}, {
		name: "spread podIP load",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:   types.NamespacedName{testNamespace, testRevision},
			Dests: sets.NewString("128.0.0.1:1234", "128.0.0.2:1234"),
		}},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
			{Namespace: testNamespace, Name: testRevision},
		},
		wantDests: sets.NewString("128.0.0.2:1234", "128.0.0.1:1234"),
	}, {
		name: "multiple ClusterIP requests",
		revisions: []*v1alpha1.Revision{
			revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1),
		},
		initUpdates: []RevisionDestsUpdate{{
			Rev:           types.NamespacedName{testNamespace, testRevision},
			ClusterIPDest: "129.0.0.1:1234",
			Dests:         sets.NewString("128.0.0.1:1234", "128.0.0.2:1234"),
		}},
		trys: []types.NamespacedName{
			{Namespace: testNamespace, Name: testRevision},
			{Namespace: testNamespace, Name: testRevision},
		},
		wantDests: sets.NewString("129.0.0.1:1234"),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			updateCh := make(chan RevisionDestsUpdate, 2)

			params := queue.BreakerParams{
				QueueDepth:      1,
				MaxConcurrency:  defaultMaxConcurrency,
				InitialCapacity: 0,
			}

			fake := kubefake.NewSimpleClientset()
			informer := kubeinformers.NewSharedInformerFactory(fake, 0)
			endpoints := informer.Core().V1().Endpoints()

			servfake := servingfake.NewSimpleClientset()
			servinginformer := servinginformers.NewSharedInformerFactory(servfake, 0)
			revisions := servinginformer.Serving().V1alpha1().Revisions()

			stopCh := make(chan struct{})
			defer close(stopCh)
			controller.StartInformers(stopCh, endpoints.Informer(), revisions.Informer())

			// Add the revision were testing
			for _, rev := range tc.revisions {
				servfake.ServingV1alpha1().Revisions(rev.Namespace).Create(rev)
				revisions.Informer().GetIndexer().Add(rev)
			}

			throttler := NewThrottler(params, revisions, endpoints, TestLogger(t))
			for _, update := range tc.initUpdates {
				updateCh <- update
			}
			close(updateCh)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				throttler.Run(updateCh)
			}()

			// Wait for throttler to complete processing updates and exit
			wg.Wait()

			for _, delRev := range tc.deletes {
				servfake.ServingV1alpha1().Revisions(delRev.Namespace).Delete(delRev.Name, nil)
				revisions.Informer().GetIndexer().Delete(delRev)
			}

			// Make sure our informer event has fired
			if len(tc.deletes) > 0 {
				time.Sleep(200 * time.Millisecond)
			}

			tryContext, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
			defer cancel()

			gotTries := tryThrottler(throttler, tc.trys, tryContext)
			gotDests := sets.NewString()
			for _, tr := range gotTries {
				gotDests.Insert(tr.Dest)
			}

			if got, want := gotDests, tc.wantDests; !got.Equal(want) {
				t.Errorf("Dests = %v, want: %v, diff: %s", got, want, cmp.Diff(want, got))
			}
		})
	}
}

func TestMultipleActivator(t *testing.T) {
	defer ClearAll()
	fake := kubefake.NewSimpleClientset()
	informer := kubeinformers.NewSharedInformerFactory(fake, 0)
	endpoints := informer.Core().V1().Endpoints()

	servfake := servingfake.NewSimpleClientset()
	servinginformer := servinginformers.NewSharedInformerFactory(servfake, 0)
	revisions := servinginformer.Serving().V1alpha1().Revisions()

	stopCh := make(chan struct{})
	defer close(stopCh)
	controller.StartInformers(stopCh, endpoints.Informer(), revisions.Informer())

	rev := revision(types.NamespacedName{testNamespace, testRevision}, networking.ProtocolHTTP1)
	// Add the revision were testing
	servfake.ServingV1alpha1().Revisions(rev.Namespace).Create(rev)
	revisions.Informer().GetIndexer().Add(rev)

	params := queue.BreakerParams{
		QueueDepth:      1,
		MaxConcurrency:  defaultMaxConcurrency,
		InitialCapacity: 0,
	}

	throttler := NewThrottler(params, revisions, endpoints, TestLogger(t))

	revID := types.NamespacedName{testNamespace, testRevision}
	possibleDests := sets.NewString("128.0.0.1:1234", "128.0.0.2:1234", "128.0.0.23:1234")
	updateCh := make(chan RevisionDestsUpdate, 1)
	updateCh <- RevisionDestsUpdate{
		Rev:   revID,
		Dests: possibleDests,
	}
	close(updateCh)
	throttler.Run(updateCh)

	// Add activator endpoint with 2 activators
	activatorEp := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networking.ActivatorServiceName,
			Namespace: system.Namespace(),
		},
		Subsets: []corev1.EndpointSubset{*epSubset(999, "http", []string{"130.0.0.1", "130.0.0.2"})},
	}
	fake.CoreV1().Endpoints(system.Namespace()).Create(activatorEp)
	endpoints.Informer().GetIndexer().Add(activatorEp)

	// Make sure our informer event has fired
	time.Sleep(200 * time.Millisecond)

	// Test with 2 activators, 3 endpoints we can send 1 request
	func() {
		var cancel context.CancelFunc
		tryContext, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
		defer cancel()

		results := tryThrottler(throttler, []types.NamespacedName{revID, revID}, tryContext)
		if !possibleDests.Has(results[0].Dest) {
			t.Errorf("Request went to an unknown destination: %s, possibles: %v", results[0].Dest, possibleDests)
		}
		if got, want := results[1].ErrString, context.DeadlineExceeded.Error(); got != want {
			t.Errorf("Error = %s, want: %s", got, want)
		}
	}()
}

func tryThrottler(throttler *Throttler, trys []types.NamespacedName, ctx context.Context) []tryResult {
	resCh := make(chan tryResult)
	defer close(resCh)
	var tryWaitg sync.WaitGroup
	tryWaitg.Add(len(trys))
	for _, revID := range trys {
		go func(revID types.NamespacedName) {
			err := throttler.Try(ctx, revID, func(dest string) error {
				tryWaitg.Done()
				resCh <- tryResult{Dest: dest}
				return nil
			})
			if err != nil {
				tryWaitg.Done()
				resCh <- tryResult{ErrString: err.Error()}
			}
		}(revID)
	}

	tryWaitg.Wait()

	ret := make([]tryResult, len(trys))
	for i := range trys {
		ret[i] = <-resCh
	}
	return ret
}

func TestInfiniteBreaker(t *testing.T) {
	defer ClearAll()
	b := &InfiniteBreaker{
		broadcast: make(chan struct{}),
		logger:    TestLogger(t),
	}

	// Verify initial condition.
	if got, want := b.Capacity(), 0; got != want {
		t.Errorf("Cap=%d, want: %d", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Maybe(ctx, nil); err == nil {
		t.Error("Should have failed, but didn't")
	}

	b.UpdateConcurrency(1)
	if got, want := b.Capacity(), 1; got != want {
		t.Errorf("Cap=%d, want: %d", got, want)
	}

	// Verify we call the thunk when we have achieved capacity.
	// Twice.
	for i := 0; i < 2; i++ {
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
		res := false
		if err := b.Maybe(ctx, func() { res = true }); err != nil {
			t.Error("Should have succeeded, but didn't")
		}
		if !res {
			t.Error("thunk was not invoked")
		}
	}

	// Scale to zero
	b.UpdateConcurrency(0)

	// Repeat initial test.
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := b.Maybe(ctx, nil); err == nil {
		t.Error("Should have failed, but didn't")
	}
	if got, want := b.Capacity(), 0; got != want {
		t.Errorf("Cap=%d, want: %d", got, want)
	}

	// And now do the async test.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	// Unlock the channel after a short delay.
	go func() {
		time.Sleep(10 * time.Millisecond)
		b.UpdateConcurrency(1)
	}()
	res := false
	if err := b.Maybe(ctx, func() { res = true }); err != nil {
		t.Error("Should have succeeded, but didn't")
	}
	if !res {
		t.Error("thunk was not invoked")
	}
}
