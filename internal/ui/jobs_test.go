package ui

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestJobStartAndPollUntilDone(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)

	release := make(chan struct{})
	statusSet := make(chan struct{})
	id := r.start(func(setStatus func(string)) (any, error) {
		setStatus("working…")
		close(statusSet)
		<-release
		return "the result", nil
	})

	select {
	case <-statusSet:
	case <-time.After(2 * time.Second):
		t.Fatalf("job never reported its status")
	}

	st, ok := r.get(id)
	if !ok {
		t.Fatalf("expected job %q to exist", id)
	}
	if st.Done {
		t.Fatalf("expected job to still be running, got done=true")
	}
	if st.Log != "working…" {
		t.Fatalf("expected status log %q, got %q", "working…", st.Log)
	}

	close(release)

	deadline := time.After(2 * time.Second)
	for {
		st, ok = r.get(id)
		if !ok {
			t.Fatalf("job %q disappeared before finishing", id)
		}
		if st.Done {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if st.Error != "" {
		t.Fatalf("expected no error, got %q", st.Error)
	}
	if st.Result != "the result" {
		t.Fatalf("expected result %q, got %v", "the result", st.Result)
	}
}

func TestJobError(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)
	id := r.start(func(setStatus func(string)) (any, error) {
		return nil, errors.New("boom")
	})

	deadline := time.After(2 * time.Second)
	for {
		st, ok := r.get(id)
		if !ok {
			t.Fatalf("job %q disappeared", id)
		}
		if st.Done {
			if st.Error != "boom" {
				t.Fatalf("expected error %q, got %q", "boom", st.Error)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("job never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestGetUnknownJob(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)
	if _, ok := r.get("does-not-exist"); ok {
		t.Fatalf("expected unknown job id to report ok=false")
	}
}

func TestStartInstallRejectsConcurrent(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)

	release := make(chan struct{})
	started := make(chan struct{})
	id1, err := r.startInstall(func(setStatus func(string)) (any, error) {
		close(started)
		<-release
		return "first", nil
	})
	if err != nil {
		t.Fatalf("expected first install to start, got error: %v", err)
	}
	<-started

	_, err = r.startInstall(func(setStatus func(string)) (any, error) {
		t.Fatalf("second install must not run while the first is in progress")
		return nil, nil
	})
	if err == nil {
		t.Fatalf("expected the second concurrent install to be rejected")
	}

	close(release)

	deadline := time.After(2 * time.Second)
	for {
		st, ok := r.get(id1)
		if !ok {
			t.Fatalf("job %q disappeared", id1)
		}
		if st.Done {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("first install never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Now that the first install released the lock, a new install
	// should be free to start.
	var wg sync.WaitGroup
	wg.Add(1)
	id2, err := r.startInstall(func(setStatus func(string)) (any, error) {
		defer wg.Done()
		return "second", nil
	})
	if err != nil {
		t.Fatalf("expected a new install to start once the first finished, got error: %v", err)
	}
	wg.Wait()
	deadline = time.After(2 * time.Second)
	for {
		st, ok := r.get(id2)
		if !ok {
			t.Fatalf("job %q disappeared", id2)
		}
		if st.Done {
			if st.Result != "second" {
				t.Fatalf("expected result %q, got %v", "second", st.Result)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("second install never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestJobExpiry(t *testing.T) {
	r := newJobRegistry(20 * time.Millisecond)
	// Freeze the clock so we control exactly when the job "finished".
	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	id := r.start(func(setStatus func(string)) (any, error) {
		return "done", nil
	})

	deadline := time.After(2 * time.Second)
	for {
		st, ok := r.get(id)
		if !ok {
			t.Fatalf("job %q disappeared before it even finished", id)
		}
		if st.Done {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Move the clock past the TTL and trigger a sweep (start does this
	// as a side effect; a direct call is clearer here).
	r.now = func() time.Time { return frozen.Add(1 * time.Hour) }
	r.sweep()

	if _, ok := r.get(id); ok {
		t.Fatalf("expected job %q to have expired and been swept", id)
	}
}
