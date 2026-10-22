package faces

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

const ns = `xmlns="http://www.w3.org/2000/svg"`

// A clean face passes, and the store keeps the tree as the writer wrote it.
func TestSanitizeKeepsACleanFace(t *testing.T) {
	in := `<?xml version="1.0" encoding="UTF-8"?>
<!-- drawn by hand -->
<svg ` + ns + ` xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 64 64" width="64" height="64" xml:space="preserve">
  <title>A fox &amp; glasses</title>
  <defs>
    <linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#f80"/><stop offset="1" stop-color="#a40"/></linearGradient>
    <clipPath id="c"><circle cx="32" cy="32" r="30"/></clipPath>
  </defs>
  <style type="text/css">.eye { fill: #222; } g > rect { opacity: 0.5 }</style>
  <g clip-path="url(#c)" transform="rotate(5 32 32)">
    <rect width="64" height="64" fill="url('#g')"/>
    <circle class="eye" cx="22" cy="28" r="4" style="stroke: black; stroke-width: 1"/>
    <path d="M10 50 L54 50" stroke="#000" stroke-linecap="round"/>
    <text x="32" y="60" text-anchor="middle" font-size="6"><tspan>fox</tspan></text>
  </g>
</svg>`
	out, err := Sanitize([]byte(in))
	if err != nil {
		t.Fatalf("a clean face is refused: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64" xml:space="preserve">`,
		`<title>A fox &amp; glasses</title>`,
		`fill="url(&#39;#g&#39;)"`,
		`g &gt; rect`,
		`clip-path="url(#c)"`,
		`<tspan>fox</tspan>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the face lacks %s:\n%s", want, s)
		}
	}
	for _, gone := range []string{"<?xml", "<!--", "drawn by hand", "xmlns:xlink"} {
		if strings.Contains(s, gone) {
			t.Errorf("the face keeps %s:\n%s", gone, s)
		}
	}
	// What the writer produced passes again, the same.
	again, err := Sanitize(out)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("the face changes on a second pass: %v\n%s\n%s", err, out, again)
	}
	// It is well-formed XML.
	d := xml.NewDecoder(bytes.NewReader(out))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("the face does not parse: %v", err)
		}
	}
}

// Each hostile face is refused, and the reason names what offends.
func TestSanitizeRefusesHostileFaces(t *testing.T) {
	cases := []struct{ name, in, reason string }{
		{"script", `<svg ` + ns + `><script>alert(1)</script></svg>`, "<script>"},
		{"script in another namespace", `<svg ` + ns + ` xmlns:h="http://www.w3.org/1999/xhtml"><h:script>alert(1)</h:script></svg>`, "<script>"},
		{"Script in another case", `<svg ` + ns + `><Script>alert(1)</Script></svg>`, "<Script>"},
		{"onload", `<svg ` + ns + ` onload="alert(1)"></svg>`, "onload"},
		{"onLoad", `<svg ` + ns + ` onLoad="alert(1)"></svg>`, "onLoad"},
		{"ONCLICK on a shape", `<svg ` + ns + `><rect ONCLICK="x" width="1"/></svg>`, "ONCLICK"},
		{"onload in a namespace", `<svg ` + ns + ` xmlns:e="urn:e"><rect e:onload="x"/></svg>`, "urn:e:onload"},
		{"xlink:href", `<svg ` + ns + ` xmlns:xlink="http://www.w3.org/1999/xlink"><rect xlink:href="#x"/></svg>`, "xlink:href"},
		{"href", `<svg ` + ns + `><linearGradient href="https://evil.example/g.svg#g"/></svg>`, "href"},
		{"HREF", `<svg ` + ns + `><rect HREF="javascript:alert(1)"/></svg>`, "HREF"},
		{"a", `<svg ` + ns + `><a><rect width="1"/></a></svg>`, "<a>"},
		{"use", `<svg ` + ns + `><use/></svg>`, "<use>"},
		{"image", `<svg ` + ns + `><image width="1"/></svg>`, "<image>"},
		{"nested foreignObject", `<svg ` + ns + `><g><g><foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject></g></g></svg>`, "<foreignObject>"},
		{"set", `<svg ` + ns + `><set attributeName="fill" to="red"/></svg>`, "<set>"},
		{"animate", `<svg ` + ns + `><animate attributeName="href"/></svg>`, "<animate>"},
		{"an element outside the list", `<svg ` + ns + `><filter id="f"/></svg>`, "<filter>"},
		{"an attribute outside the list", `<svg ` + ns + `><rect data-x="1"/></svg>`, "data-x"},
		{"style with url(", `<svg ` + ns + `><rect style="fill: url(https://evil.example/x)"/></svg>`, "url("},
		{"style with URL(", `<svg ` + ns + `><rect style="background: URL(x)"/></svg>`, "url("},
		{"style element with url(", `<svg ` + ns + `><style>rect { fill: url(#g) }</style></svg>`, "url("},
		{"style with @import", `<svg ` + ns + `><style>@import "https://evil.example/x.css";</style></svg>`, "@import"},
		{"style with @IMPORT", `<svg ` + ns + `><style>@IMPORT 'x.css';</style></svg>`, "@import"},
		{"style with expression(", `<svg ` + ns + `><rect style="width: expression(alert(1))"/></svg>`, "expression("},
		{"style with an escape", `<svg ` + ns + `><rect style="fill: u\72l(x)"/></svg>`, "backslash"},
		{"style with image-set(", `<svg ` + ns + `><rect style="fill: image-set('x' 1x)"/></svg>`, "image-set("},
		{"style with an entity that spells url(", `<svg ` + ns + `><rect style="fill: &#117;rl(x)"/></svg>`, "url("},
		{"fill with a remote url", `<svg ` + ns + `><rect fill="url(https://evil.example/x.svg#g)"/></svg>`, "outside the face"},
		{"url( in another attribute", `<svg ` + ns + `><rect stroke-width="url(#g)"/></svg>`, "stroke-width"},
		{"an entity", `<!DOCTYPE svg [<!ENTITY x "boom">]><svg ` + ns + `>&x;</svg>`, "DOCTYPE"},
		{"an undefined entity", `<svg ` + ns + `>&nbsp;</svg>`, "&nbsp;"},
		{"a processing instruction", `<?xml-stylesheet href="https://evil.example/x.css"?><svg ` + ns + `></svg>`, "<?xml-stylesheet?>"},
		{"a processing instruction inside", `<svg ` + ns + `><?php echo 1; ?></svg>`, "<?php?>"},
		{"a late XML declaration", `<svg ` + ns + `></svg><?xml version="1.0"?>`, "<?xml?>"},
		{"another namespace", `<svg xmlns="http://www.w3.org/1999/xhtml"></svg>`, "namespace http://www.w3.org/1999/xhtml"},
		{"an HTML root", `<html><svg ` + ns + `/></html>`, "<html>"},
		{"two roots", `<svg ` + ns + `/><svg ` + ns + `/>`, "second root"},
		{"text outside", `<svg ` + ns + `/>tail`, "outside the svg"},
		{"no svg", `   `, "no svg"},
		{"not XML", `<svg ` + ns + `><rect></svg>`, "not well-formed"},
		{"another encoding", `<?xml version="1.0" encoding="ISO-8859-1"?><svg ` + ns + `/>`, "not well-formed"},
		{"too large", `<svg ` + ns + `>` + strings.Repeat(" ", MaxSVGBytes) + `</svg>`, "at most 32 KB"},
		{"too deep", `<svg ` + ns + `>` + strings.Repeat("<g>", 40) + strings.Repeat("</g>", 40) + `</svg>`, "deep"},
	}
	for _, c := range cases {
		out, err := Sanitize([]byte(c.in))
		var ref *Refusal
		if !errors.As(err, &ref) || !errors.Is(err, ErrRefused) {
			t.Errorf("%s: passed as %s (%v)", c.name, out, err)
			continue
		}
		if !strings.Contains(ref.Reason, c.reason) {
			t.Errorf("%s: the reason %q does not name %q", c.name, ref.Reason, c.reason)
		}
	}
}

// A face whose rewrite is larger than the bound is refused.
func TestSanitizeBoundsTheRewrite(t *testing.T) {
	// A quote in a text takes five bytes once it is written again.
	in := `<svg ` + ns + `><text>` + strings.Repeat(`"`, 10000) + `</text></svg>`
	if len(in) > MaxSVGBytes {
		t.Fatalf("the input has %d bytes", len(in))
	}
	if _, err := Sanitize([]byte(in)); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "written again") {
		t.Fatalf("Sanitize = %v", err)
	}
}

func TestIdenticon(t *testing.T) {
	a := Identicon("reviewer")
	if !bytes.Equal(a, Identicon("reviewer")) {
		t.Fatal("the identicon of one name changes")
	}
	if bytes.Equal(a, Identicon("migrator")) {
		t.Fatal("two names give one identicon")
	}
	if !bytes.Contains(a, []byte("<title>reviewer</title>")) || len(a) > 4096 {
		t.Fatalf("the identicon = %s", a)
	}
	if out, err := Sanitize(a); err != nil || !bytes.Equal(out, a) {
		t.Fatalf("the identicon does not pass the sanitizer as it is: %v\n%s\n%s", err, a, out)
	}
	if got := Identicon(`a<b>&"c`); !bytes.Contains(got, []byte("<title>a&lt;b&gt;&amp;&#34;c</title>")) {
		t.Fatalf("the title is not escaped: %s", got)
	}
	// The pattern is symmetric: each cell of a column has its mirror.
	for _, name := range []string{"reviewer", "migrator", "manager", "x"} {
		cells := cellsOf(t, Identicon(name))
		if len(cells) == 0 {
			t.Fatalf("%s has no cell", name)
		}
		for c := range cells {
			if !cells[[2]int{4 - c[0], c[1]}] {
				t.Errorf("%s: the cell %v has no mirror", name, c)
			}
		}
	}
}

// cellsOf reads the cells of an identicon from its path, as column and row.
func cellsOf(t *testing.T, svg []byte) map[[2]int]bool {
	t.Helper()
	s := string(svg)
	i := strings.Index(s, ` d="`)
	if i < 0 {
		t.Fatalf("no path: %s", s)
	}
	d := s[i+4:]
	d = d[:strings.IndexByte(d, '"')]
	out := map[[2]int]bool{}
	for part := range strings.SplitSeq(strings.TrimSuffix(d, "z"), "z") {
		var x, y, w, h, back int
		if _, err := fmt.Sscanf(part, "M%d %dh%dv%dh-%d", &x, &y, &w, &h, &back); err != nil {
			t.Fatalf("the part %q: %v", part, err)
		}
		out[[2]int{(x - iconMargin) / iconCell, (y - iconMargin) / iconCell}] = true
	}
	return out
}
