package pools

import (
	"strings"
	"testing"
)

// frontEntry adds the entry front of tools.mcp to the fixture, as a pool
// that reaches the MCP server of the front has it.
func frontEntry(extra string) string {
	return strings.Replace(fixture, `        mcp:
          - name: tickets`, extra+`        mcp:
          - name: front
            endpoint: http://fw-front.fw-fleet.svc.cluster.local:8080/mcp
            auth: {bearer: FW_API_TOKEN}
            trusted: true
          - name: tickets`, 1)
}

// The overlay of a pool whose workers reach the MCP server of the front
// sets tools.memory.front to the address of the front and the variable of
// the static token, and the hash covers it. A pool without the entry, and
// a pool that sets tools.memory.front itself, keep what they have.
func TestOverlaySetsTheMemoryFront(t *testing.T) {
	base := loadFixture(t, frontEntry(""))[0]
	merged, err := base.WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	wc := workerOf(t, merged)
	f := wc.Tools.Memory.Front
	if f == nil || f.URL != "http://fw-front.fw-fleet.svc.cluster.local:8080" || f.TokenEnv != "FW_API_TOKEN" {
		t.Fatalf("tools.memory.front = %+v", f)
	}
	if workerOf(t, base).Tools.Memory.Front != nil || merged.Hash() == base.Hash() {
		t.Fatal("the configuration of the pool alone sets the front of the notes, or the hash did not follow")
	}
	// The scope of the notes stays.
	scoped := loadFixture(t, frontEntry("        memory: {scope: principal}\n"))[0]
	if m, err := scoped.WithOverlay(Overlay{}); err != nil || workerOf(t, m).Tools.Memory.Scope != "principal" || workerOf(t, m).Tools.Memory.Front == nil {
		t.Fatalf("a pool with a scope = %+v, %v", workerOf(t, m).Tools.Memory, err)
	}

	plain := loadFixture(t, fixture)[0]
	if m, err := plain.WithOverlay(Overlay{}); err != nil || workerOf(t, m).Tools.Memory.Front != nil {
		t.Fatalf("a pool without the front got the front of the notes: %v", err)
	}
	own := loadFixture(t, frontEntry("        memory: {front: {url: \"http://other:8080\", tokenEnv: OTHER_TOKEN}}\n"))[0]
	m, err := own.WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	if f := workerOf(t, m).Tools.Memory.Front; f == nil || f.URL != "http://other:8080" || f.TokenEnv != "OTHER_TOKEN" {
		t.Fatalf("the front of the pool = %+v", f)
	}
}
