package faces

// This file is the sanitizer of a face (docs/ARCHITECTURE.md section
// 16.2). It parses the SVG with encoding/xml and writes the tree again
// from what the parser saw, so the store holds only what the writer
// produced. It keeps a short list of elements and attributes. A face with
// anything else is refused, with a reason that names the first element or
// attribute that offends, and is never cleaned in silence.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// MaxSVGBytes bounds a face, before and after the sanitizer.
const MaxSVGBytes = 32 << 10

// Bounds of the tree of a face.
const (
	maxDepth    = 32
	maxElements = 2048
	maxAttrs    = 64
)

// Namespaces of a face.
const (
	svgNS   = "http://www.w3.org/2000/svg"
	xlinkNS = "http://www.w3.org/1999/xlink"
	xmlNS   = "http://www.w3.org/XML/1998/namespace"
)

// ErrRefused is the error of a face that the sanitizer refuses. The error
// is a *Refusal with the reason.
var ErrRefused = errors.New("faces: the sanitizer refuses the face")

// Refusal says why the sanitizer refuses a face.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string        { return "faces: the sanitizer refuses the face: " + r.Reason }
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// keptElements are the elements of a face. The element style stays too,
// under the rule of a style (styleRule).
var keptElements = map[string]bool{
	"svg": true, "g": true, "path": true, "circle": true, "ellipse": true, "rect": true,
	"line": true, "polyline": true, "polygon": true, "text": true, "tspan": true,
	"defs": true, "linearGradient": true, "radialGradient": true, "stop": true,
	"clipPath": true, "mask": true, "title": true, "desc": true, "style": true,
}

// refusedElements get a reason of their own, because they run code, load
// a resource, or link out.
var refusedElements = map[string]string{
	"script":        "runs code",
	"foreignObject": "embeds HTML",
	"use":           "copies or loads other content",
	"image":         "loads an image",
	"a":             "links out",
	"iframe":        "embeds a page",
	"animate":       "changes attributes at run time",
	"set":           "changes attributes at run time",
}

// keptAttrs are the attributes of a face: the presentation attributes
// (cssAttrs), the geometry of the kept elements, viewBox, width, height,
// id, class, transform, and style under its rule.
var keptAttrs = map[string]bool{}

// cssAttrs are the presentation attributes, which a browser reads as CSS
// values.
var cssAttrs = map[string]bool{}

func init() {
	for _, a := range strings.Fields(`
		alignment-baseline baseline-shift clip clip-path clip-rule color color-interpolation
		color-rendering direction display dominant-baseline fill fill-opacity fill-rule
		font-family font-size font-size-adjust font-stretch font-style font-variant font-weight
		letter-spacing mask opacity overflow paint-order shape-rendering stop-color stop-opacity
		stroke stroke-dasharray stroke-dashoffset stroke-linecap stroke-linejoin stroke-miterlimit
		stroke-opacity stroke-width text-anchor text-decoration text-rendering transform
		transform-origin unicode-bidi vector-effect visibility word-spacing writing-mode`) {
		keptAttrs[a] = true
		cssAttrs[a] = true
	}
	for _, a := range strings.Fields(`
		viewBox width height x y x1 y1 x2 y2 cx cy r rx ry fx fy fr d points pathLength
		dx dy rotate textLength lengthAdjust offset gradientUnits gradientTransform spreadMethod
		clipPathUnits maskUnits maskContentUnits preserveAspectRatio id class style version lang`) {
		keptAttrs[a] = true
	}
}

// refAttrs are the attributes that can name a gradient, a clip path, or a
// mask of the face itself with url(#id).
var refAttrs = map[string]bool{"fill": true, "stroke": true, "clip-path": true, "mask": true}

// cssLoads are the parts of CSS that load a resource or run code. A style
// with one of them is refused.
var cssLoads = []string{"url(", "@import", "expression(", "src(", "image(", "image-set(", "cross-fade(", "element(", "-moz-binding", "behavior"}

// Sanitize checks an SVG and returns the face that the store keeps: the
// tree that the parser saw, written again, with only the kept elements and
// attributes, without comments, the XML declaration, and the namespace
// declarations beside the one of SVG. It refuses with a *Refusal a face
// above MaxSVGBytes, a face that is not well-formed XML, a DOCTYPE or an
// entity, a processing instruction, an element or an attribute outside the
// lists, an event attribute such as onload in any case, an href in any
// namespace, a style with url(, @import, expression(, a function that
// loads a resource, or a backslash, a reference to anything but #id of the
// face itself, and a tree that is too large.
func Sanitize(svg []byte) ([]byte, error) {
	if len(svg) > MaxSVGBytes {
		return nil, refuse("the face has %d KB; a face holds at most %d KB", (len(svg)+1023)>>10, MaxSVGBytes>>10)
	}
	d := xml.NewDecoder(bytes.NewReader(svg))
	d.Strict = true
	var out bytes.Buffer
	var stack []string
	// css gathers the whole text of the open style element. A comment and
	// a CDATA section split the text into tokens, and the writer joins
	// them, so the rule reads the text as the writer writes it, at the end
	// of the element.
	var css strings.Builder
	elements, rootDone, seen := 0, false, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			var se *xml.SyntaxError
			if errors.As(err, &se) {
				return nil, refuse("the face is not well-formed XML at line %d: %s", se.Line, se.Msg)
			}
			return nil, refuse("the face is not well-formed XML: %s", err.Error())
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			if t.Target != "xml" || seen {
				return nil, refuse("the processing instruction <?%s?> is not allowed in a face", t.Target)
			}
		case xml.Directive:
			word, _, _ := strings.Cut(strings.TrimSpace(string(t)), " ")
			return nil, refuse("the declaration <!%s> is not allowed in a face; a face holds no DOCTYPE and no entity", word)
		case xml.Comment:
			seen = true
		case xml.CharData:
			seen = true
			if len(stack) == 0 {
				if strings.TrimSpace(string(t)) != "" {
					return nil, refuse("the face holds text outside the svg element")
				}
				continue
			}
			if stack[len(stack)-1] == "style" {
				css.Write(t)
			}
			if err := xml.EscapeText(&out, t); err != nil {
				return nil, refuse("the text of the face does not encode: %s", err.Error())
			}
		case xml.StartElement:
			seen = true
			name := t.Name.Local
			if rootDone {
				return nil, refuse("the face has a second root element <%s>; a face has one svg element", name)
			}
			if len(stack) == 0 && name != "svg" {
				return nil, refuse("the root element is <%s>; a face starts with <svg>", name)
			}
			if len(stack) > 0 && stack[len(stack)-1] == "style" {
				return nil, refuse("the element <%s> is inside <style>; a style element holds text alone", name)
			}
			if t.Name.Space != "" && t.Name.Space != svgNS {
				return nil, refuse("the element <%s> is in the namespace %s; a face holds SVG elements only", name, t.Name.Space)
			}
			if why, ok := refusedElements[name]; ok {
				return nil, refuse("the element <%s> is not allowed in a face, because it %s", name, why)
			}
			if !keptElements[name] {
				return nil, refuse("the element <%s> is not allowed in a face; a face keeps %s", name, keptList())
			}
			if elements++; elements > maxElements {
				return nil, refuse("the face has more than %d elements", maxElements)
			}
			if len(stack) >= maxDepth {
				return nil, refuse("the elements of the face nest more than %d deep", maxDepth)
			}
			if len(t.Attr) > maxAttrs {
				return nil, refuse("the element <%s> has %d attributes; an element of a face has at most %d", name, len(t.Attr), maxAttrs)
			}
			out.WriteString("<" + name)
			if len(stack) == 0 {
				out.WriteString(` xmlns="` + svgNS + `"`)
			}
			written := map[string]bool{}
			for _, a := range t.Attr {
				keep, err := checkAttr(name, a)
				if err != nil {
					return nil, err
				}
				if keep == "" {
					continue
				}
				if written[keep] {
					return nil, refuse("the attribute %s of <%s> appears twice", keep, name)
				}
				written[keep] = true
				out.WriteString(" " + keep + `="`)
				if err := xml.EscapeText(&out, []byte(a.Value)); err != nil {
					return nil, refuse("the attribute %s of <%s> does not encode: %s", keep, name, err.Error())
				}
				out.WriteString(`"`)
			}
			out.WriteString(">")
			stack = append(stack, name)
			if name == "style" {
				css.Reset()
			}
		case xml.EndElement:
			name := stack[len(stack)-1]
			if name == "style" {
				if err := styleRule("the style element", css.String()); err != nil {
					return nil, err
				}
			}
			stack = stack[:len(stack)-1]
			out.WriteString("</" + name + ">")
			if len(stack) == 0 {
				rootDone = true
			}
		}
	}
	if !rootDone {
		return nil, refuse("the face holds no svg element")
	}
	if out.Len() > MaxSVGBytes {
		return nil, refuse("the face has %d KB once it is written again; a face holds at most %d KB", (out.Len()+1023)>>10, MaxSVGBytes>>10)
	}
	return out.Bytes(), nil
}

// checkAttr checks one attribute of the element el. It returns the name to
// write, "" for a namespace declaration that the writer leaves out, or a
// refusal.
func checkAttr(el string, a xml.Attr) (string, error) {
	space, local := a.Name.Space, a.Name.Local
	lower := strings.ToLower(local)
	shown := local
	switch space {
	case "":
	case xlinkNS:
		shown = "xlink:" + local
	case xmlNS:
		shown = "xml:" + local
	case "xmlns":
		shown = "xmlns:" + local
	default:
		shown = space + ":" + local
	}
	switch {
	case space == "" && local == "xmlns":
		if a.Value != svgNS {
			return "", refuse("the attribute xmlns of <%s> names %q; a face is in the namespace %s", el, a.Value, svgNS)
		}
		return "", nil
	case space == "xmlns":
		// A declaration of a prefix holds no content. An element or an
		// attribute that uses the prefix is refused on its own.
		return "", nil
	case strings.HasPrefix(lower, "on"):
		return "", refuse("the event attribute %s of <%s> runs code; a face has no event attribute", shown, el)
	case lower == "href":
		return "", refuse("the attribute %s of <%s> links or loads a resource; a face has no href", shown, el)
	case space == xmlNS && (local == "space" || local == "lang"):
		return shown, nil
	case space != "":
		return "", refuse("the attribute %s of <%s> is not allowed in a face", shown, el)
	case el == "style" && local == "type":
		return local, nil
	case !keptAttrs[local]:
		return "", refuse("the attribute %s of <%s> is not allowed in a face; a face keeps the presentation attributes, the geometry of its shapes, viewBox, width, height, id, class, transform, and style", shown, el)
	case local == "style":
		return local, styleRule(fmt.Sprintf("the style attribute of <%s>", el), a.Value)
	case cssAttrs[local]:
		// A browser reads a presentation attribute as a CSS value, so the
		// rule of a style holds for it too, beside url(#id).
		if err := cssRule(fmt.Sprintf("the attribute %s of <%s>", shown, el), a.Value, true); err != nil {
			return "", err
		}
	}
	if strings.Contains(strings.ToLower(a.Value), "url(") {
		if !refAttrs[local] {
			return "", refuse("the attribute %s of <%s> holds url(; only fill, stroke, clip-path, and mask can name an element of the face with url(#id)", shown, el)
		}
		if !localRefs(a.Value) {
			return "", refuse("the attribute %s of <%s> names something outside the face; url( can only name an element of the face, as url(#id)", shown, el)
		}
	}
	return local, nil
}

// styleRule refuses a style that loads a resource or runs code: url(,
// @import, expression(, another function that loads a resource, or a
// backslash, which can spell any of them in an escape.
func styleRule(what, css string) error { return cssRule(what, css, false) }

// cssRule is the rule of a style for a CSS text. With refs, url( stays
// for the check of localRefs, as a presentation attribute needs it.
func cssRule(what, css string, refs bool) error {
	if strings.Contains(css, `\`) {
		return refuse("%s holds a backslash; a style of a face has no escape", what)
	}
	lower := strings.ToLower(css)
	for _, bad := range cssLoads {
		if refs && bad == "url(" {
			continue
		}
		if strings.Contains(lower, bad) {
			return refuse("%s holds %s; a style of a face loads nothing", what, bad)
		}
	}
	return nil
}

// localRefs reports whether each url( of a value names an element of the
// face: url(#id), with optional spaces and quotes.
func localRefs(v string) bool {
	rest := strings.ToLower(v)
	for {
		i := strings.Index(rest, "url(")
		if i < 0 {
			return true
		}
		rest = rest[i+4:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return false
		}
		ref := strings.TrimSpace(rest[:end])
		if len(ref) >= 2 && (ref[0] == '"' || ref[0] == '\'') && ref[len(ref)-1] == ref[0] {
			ref = ref[1 : len(ref)-1]
		}
		id, ok := strings.CutPrefix(ref, "#")
		if !ok || id == "" || strings.IndexFunc(id, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.'
		}) >= 0 {
			return false
		}
		rest = rest[end+1:]
	}
}

// keptList names the kept elements for a refusal.
func keptList() string {
	return "svg, g, path, circle, ellipse, rect, line, polyline, polygon, text, tspan, defs, linearGradient, radialGradient, stop, clipPath, mask, title, desc, and style"
}
