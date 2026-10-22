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
	if err != nil || f.Custom || f.Hash != faceHash(Identicon("reviewer"), "", "") || string(f.SVG) != string(Identicon("reviewer")) {
		t.Fatalf("Get without a face = %+v, %v", f, err)
	}
	f, err = s.Put(ctx, "reviewer", []byte(fox), "alice")
	if err != nil || !f.Custom || f.By != "alice" || f.Version == 0 {
		t.Fatalf("Put = %+v, %v", f, err)
	}
	want, _ := Sanitize([]byte(fox))
	if string(f.SVG) != string(want) || f.Hash != faceHash(want, "", "") {
		t.Fatalf("the store keeps %s, not what the sanitizer wrote", f.SVG)
	}
	if _, err := s.Put(ctx, "reviewer", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`), "mallory"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Put of a hostile face = %v", err)
	}
	if got, _ := s.Get(ctx, "reviewer"); got.Hash != f.Hash {
		t.Fatalf("a refused face changed the face: %+v", got)
	}
	m, err := s.Meta(ctx, "reviewer", "  Reviewer Fox ", "Reads every diff twice.", "bob")
	// The hash covers the name and the tagline, so the page reads the face
	// again when they change.
	if err != nil || m.DisplayName != "Reviewer Fox" || m.Tagline != "Reads every diff twice." || m.Hash == f.Hash || !m.Custom ||
		m.Hash != faceHash(want, "Reviewer Fox", "Reads every diff twice.") {
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
	d, err := s.Delete(ctx, "reviewer", "alice")
	if err != nil || d.Custom || d.Hash != faceHash(Identicon("reviewer"), "Reviewer Fox", tag) {
		t.Fatalf("Delete = %+v, %v", d, err)
	}
	g, err := s.Get(ctx, "reviewer")
	if err != nil || g.Custom || g.Hash != d.Hash || g.DisplayName != "Reviewer Fox" {
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
	if _, err := s.Delete(ctx, "nobody", "alice"); err != nil {
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

// A face that something else wrote into the bucket passes the sanitizer
// again on a read, and a write repairs an entry that does not decode.
func TestStoreDistrustsTheBucket(t *testing.T) {
	ctx := context.Background()
	b := bucket.NewMem(bucketConfig)
	s := New(b, quiet())
	hostile, _ := encode(record{Hash: "x", By: "mallory"}, []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`))
	if _, err := b.Create(ctx, keyOf("reviewer"), hostile); err != nil {
		t.Fatal(err)
	}
	f, err := s.Get(ctx, "reviewer")
	if err != nil || f.Custom || strings.Contains(string(f.SVG), "onload") {
		t.Fatalf("Get of a hostile entry = %+v %s, %v", f, f.SVG, err)
	}
	if _, err := b.Create(ctx, keyOf("migrator"), []byte("no record line")); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Get(ctx, "migrator"); err != nil || f.Custom {
		t.Fatalf("Get of a broken entry = %+v, %v", f, err)
	}
	if _, err := s.Delete(ctx, "migrator", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, keyOf("migrator")); !errors.Is(err, bucket.ErrNotFound) {
		t.Fatalf("Delete left the broken entry: %v", err)
	}
	if _, err := b.Create(ctx, keyOf("migrator"), []byte("no record line")); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Put(ctx, "migrator", []byte(fox), "alice"); err != nil || !f.Custom {
		t.Fatalf("Put over a broken entry = %+v, %v", f, err)
	}
}
