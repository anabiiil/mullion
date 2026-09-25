package ui

import (
	"errors"
	"strings"
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

func waitDone(t *testing.T, r *jobRegistry, id string) JobStatus {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		st, ok := r.get(id)
		if !ok {
			t.Fatalf("job %q disappeared", id)
		}
		if st.Done {
			return st
		}
		select {
		case <-deadline:
			t.Fatalf("job never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestStreamJobReportsOutputAsLog(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)
	id := r.startStream(func(setStatus func(string), appendLog func([]byte)) (any, error) {
		setStatus("running")
		appendLog([]byte("hello "))
		appendLog([]byte("world\n"))
		return 0, nil
	})
	st := waitDone(t, r, id)
	if st.Log != "hello world\n" {
		t.Fatalf("log = %q, want the streamed output", st.Log)
	}
	if st.Status != "running" {
		t.Fatalf("status = %q, want %q", st.Status, "running")
	}
	if st.LogDropped != 0 {
		t.Fatalf("logDropped = %d, want 0", st.LogDropped)
	}
}

func TestStreamJobOutputIsBounded(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)
	line := []byte(strings.Repeat("x", 99) + "\n")
	total := 0
	id := r.startStream(func(_ func(string), appendLog func([]byte)) (any, error) {
		for total < 3*jobOutputLimit {
			appendLog(line)
			total += len(line)
		}
		appendLog([]byte("last line\n"))
		total += len("last line\n")
		return nil, nil
	})
	st := waitDone(t, r, id)
	if len(st.Log) > jobOutputLimit {
		t.Fatalf("kept %d bytes, limit is %d", len(st.Log), jobOutputLimit)
	}
	if !strings.HasSuffix(st.Log, "last line\n") {
		t.Fatalf("the newest output must be kept")
	}
	if !strings.HasPrefix(st.Log, "xxx") {
		t.Fatalf("the kept output should start at a line boundary, got %q", st.Log[:10])
	}
	if int(st.LogDropped)+len(st.Log) != total {
		t.Fatalf("dropped %d + kept %d != written %d", st.LogDropped, len(st.Log), total)
	}
}

func TestPlainJobLogStaysTheStatusLine(t *testing.T) {
	r := newJobRegistry(10 * time.Minute)
	id := r.start(func(setStatus func(string)) (any, error) {
		setStatus("step 2")
		return nil, nil
	})
	st := waitDone(t, r, id)
	if st.Log != "step 2" || st.Status != "" {
		t.Fatalf("log=%q status=%q, want the status line as log", st.Log, st.Status)
	}
}
