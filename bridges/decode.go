package bridges

// This file reads the body of a post (docs/ARCHITECTURE.md section 18.5).
// A post with files carries up to 32 MiB of them in base64. The decoder of
// the kernel (native.Kit.DecodeFiles) reads such a body through a buffer
// that grows by doubling, and allocates about 168 MB for a body of 44.7 MB.
// decodePost reads the body into one buffer of its length, and the files
// come out of that buffer with no other copy: about 78 MB for the same
// body.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// decodeSlot lets one large body decode at a time on a replica. A body
// with files holds about 2.3 times the bytes of its files while it decodes,
// so posts side by side would fill the memory of the front. The files that
// a decode gives stay under maxHeldBytes (hold).
var decodeSlot = make(chan struct{}, 1)

// decodePost reads the body of a post into spec, as native.Kit.DecodeFiles
// does: one JSON object with no unknown field, of at most
// native.MaxBodyBytes without files and native.MaxFilesBodyBytes with
// them. A body that can be larger than native.MaxBodyBytes waits for
// decodeSlot first. When it returns false, it wrote the error response.
func decodePost(w http.ResponseWriter, r *http.Request, spec *PostSpec) bool {
	if r.ContentLength > native.MaxFilesBodyBytes {
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", native.MaxFilesBodyBytes))
		return false
	}
	if r.ContentLength < 0 || r.ContentLength > native.MaxBodyBytes {
		select {
		case decodeSlot <- struct{}{}:
			defer func() { <-decodeSlot }()
		case <-r.Context().Done():
			native.WriteProblem(w, r, http.StatusServiceUnavailable, "the request ended while another upload decoded; send it again")
			return false
		}
	}
	body := http.MaxBytesReader(w, r.Body, native.MaxFilesBodyBytes)
	var buf []byte
	var err error
	if r.ContentLength >= 0 {
		buf = make([]byte, r.ContentLength)
		_, err = io.ReadFull(body, buf)
	} else {
		buf, err = io.ReadAll(body)
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", native.MaxFilesBodyBytes))
		return false
	case err != nil:
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is not readable: "+err.Error())
		return false
	case len(bytes.TrimSpace(buf)) == 0:
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is empty; send a JSON object")
		return false
	}
	if err := json.Unmarshal(buf, spec); err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is not valid: "+bodyError(err))
		return false
	}
	if key := unknownField(buf, reflect.TypeOf(spec)); key != "" {
		native.WriteProblem(w, r, http.StatusBadRequest, fmt.Sprintf("the body is not valid: json: unknown field %q", key))
		return false
	}
	if len(spec.Files) == 0 && len(buf) > native.MaxBodyBytes {
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", native.MaxBodyBytes))
		return false
	}
	return true
}

// bodyError words an error of json.Unmarshal as the decoder of the kernel
// words it.
func bodyError(err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) && strings.Contains(syn.Error(), "after top-level value") {
		return "the body holds data after the JSON object"
	}
	return err.Error()
}

// rawRef keeps a JSON value as the part of the input that holds it, with
// no copy. json.Unmarshal hands an Unmarshaler a part of its input, so a
// rawRef lives as long as that input.
type rawRef []byte

func (r *rawRef) UnmarshalJSON(b []byte) error {
	*r = b
	return nil
}

// unknownField returns the first key of the JSON value data that the type
// t has no field for, at any depth, as json.Decoder.DisallowUnknownFields
// refuses it, or "". A key matches a field as encoding/json matches it,
// without regard to case. A value of the wrong kind is left to
// json.Unmarshal.
func unknownField(data []byte, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]rawRef
		if json.Unmarshal(data, &obj) != nil {
			return ""
		}
		fields := jsonFields(t)
		for k, v := range obj {
			ft, ok := fields[strings.ToLower(k)]
			if !ok {
				return k
			}
			if key := unknownField(v, ft); key != "" {
				return key
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "" // []byte holds base64 text
		}
		var items []rawRef
		if json.Unmarshal(data, &items) != nil {
			return ""
		}
		for _, it := range items {
			if key := unknownField(it, t.Elem()); key != "" {
				return key
			}
		}
	case reflect.Map:
		var obj map[string]rawRef
		if json.Unmarshal(data, &obj) != nil {
			return ""
		}
		for _, v := range obj {
			if key := unknownField(v, t.Elem()); key != "" {
				return key
			}
		}
	}
	return ""
}

// fieldCache holds the fields of each struct type, by the lower case of
// the JSON name.
var fieldCache sync.Map

// jsonFields returns the fields of the struct type t by the lower case of
// their JSON name, as encoding/json names them, with the fields of an
// embedded struct.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	if v, ok := fieldCache.Load(t); ok {
		return v.(map[string]reflect.Type)
	}
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[strings.ToLower(name)] = f.Type
	}
	fieldCache.Store(t, out)
	return out
}
