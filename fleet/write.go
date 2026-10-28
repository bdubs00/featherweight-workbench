package fleet

// This file posts to a run route of another member for a surface of the
// front, such as the check of a result by a person (package work).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PostJSON forwards a POST of path with the JSON of body to a member, with
// the credential in ctx (ForRun), and decodes a JSON answer into out when
// out is not nil. An error answer is a *MemberError, as for ReadJSON.
func PostJSON(ctx context.Context, v View, member, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("fleet: build the request for the member %s: %w", member, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.Forward(ctx, member, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return fmt.Errorf("fleet: read the answer of the member %s: %w", member, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		me := errorOf(member, resp.StatusCode, raw)
		if env := TokenEnvOf(ctx); env != "" && (me.Status == http.StatusUnauthorized || me.Status == http.StatusForbidden) {
			me.Status, me.Detail, me.Token = http.StatusForbidden, RefusedToken(member, env, me.Status, me.Detail), true
		}
		return me
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("fleet: the answer of the member %s is not JSON: %w", member, err)
	}
	return nil
}
