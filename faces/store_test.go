package faces

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const fox = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><title>fox</title><circle cx="5" cy="5" r="4" fill="#f80"/></svg>`

func contract(t *testing.T, s *Store) {
	ctx := context.Background()
	f, err := s.Get(ctx, "reviewer")
	if err != nil || f.Stored || f.Hash != hashOf(Identicon("reviewer")) || string(f.SVG) != string(Identicon("reviewer")) {
		t.Fatalf("Get without a face = %+v, %v", f, err)
	}
	f, err = s.Put(ctx, "reviewer", []byte(fox), "alice")
	if err != nil || !f.Stored || f.By != "alice" || f.Version == 0 {
		t.Fatalf("Put = %+v, %v", f, err)
	}
	want, _ := Sanitize([]byte(fox))
	if string(f.SVG) != string(want) || f.Hash != hashOf(want) {
		t.Fatalf("the store keeps %s, not what the sanitizer wrote", f.SVG)
	}
	if _, err := s.Put(ctx, "reviewer", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`), "mallory"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Put of a hostile face = %v", err)
	}
	if got, _ := s.Get(ctx, "reviewer"); got.Hash != f.Hash {
		t.Fatalf("a refused face changed the face: %+v", got)
	}
	m, err := s.Meta(ctx, "reviewer", "  Reviewer Fox ", "Reads every diff twice.", "bob")
	if err != nil || m.DisplayName != "Reviewer Fox" || m.Tagline != "Reads every diff twice." || m.Hash != f.Hash || !m.Stored {
		t.Fatalf("Meta = %+v, %v", m, err)
	}
	if _, err := s.Meta(ctx, "reviewer", strings.Repeat("n", 65), "", "bob"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a display name of 65 characters = %v", err)
	}
	if _, err := s.Meta(ctx, "reviewer", "", strings.Repeat("t", 141), "bob"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a tagline of 141 characters = %v", err)
	}
	if _, err := s.Meta(ctx, "reviewer", "two\nlines", "", "bob"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a display name of two lines = %v", err)
	}
	tag := "Still reads twice."
	p, err := s.Patch(ctx, "reviewer", nil, &tag, "bob")
	if err != nil || p.DisplayName != "Reviewer Fox" || p.Tagline != tag {
		t.Fatalf("Patch of the tagline alone = %+v, %v", p, err)
	}
	// Delete brings back the identicon and keeps the display name.
	if err := s.Delete(ctx, "reviewer", "alice"); err != nil {
		t.Fatal(err)
	}
	g, err := s.Get(ctx, "reviewer")
	if err != nil || g.Stored || g.Hash != hashOf(Identicon("reviewer")) || g.DisplayName != "Reviewer Fox" {
		t.Fatalf("Get after Delete = %+v, %v", g, err)
	}
	// An agent with nothing left loses its entry.
	empty := ""
	if _, err := s.Patch(ctx, "reviewer", &empty, &empty, "alice"); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Get(ctx, "reviewer"); g.Version != 0 {
		t.Fatalf("an agent with nothing keeps an entry: %+v", g)
	}
	if err := s.Delete(ctx, "nobody", "alice"); err != nil {
		t.Fatalf("Delete of an agent without a face = %v", err)
	}
	for _, bad := range []string{"", ".hidden", "a/b", strings.Repeat("x", 65)} {
		if _, err := s.Get(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Get %q = %v", bad, err)
		}
	}
}

func TestStoreInMemory(t *testing.T) {
	contract(t, New(bucket.NewMem(bucketConfig), quiet()))
}

func TestStoreOnNATS(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	contract(t, New(bucket.NewNATS(js, "FW_FACES", bucketConfig), quiet()))
	kv, err := js.KeyValue(context.Background(), "FW_FACES")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := kv.Status(context.Background()); err != nil || st.History() != History {
		t.Fatalf("the bucket keeps %d values: %v", st.History(), err)
	}
}

func TestStatus(t *testing.T) {
	cfg := fw.DefaultConfig()
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "memory") {
		t.Fatalf("Status on mem = %v %q", ok, line)
	}
	cfg.Bus.Driver = "nats"
	cfg.Bus.Options = map[string]string{"stream": "AGENTS"}
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "AGENTS_FACES") {
		t.Fatalf("Status on nats = %v %q", ok, line)
	}
	cfg.Bus.Driver = "redis"
	if ok, line := Status(cfg); !ok || !strings.HasPrefix(line, "faces: off") {
		t.Fatalf("Status on redis = %v %q", ok, line)
	}
}
