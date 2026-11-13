package generic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	cloudinv "github.com/james00012/cloud-inventory"
)

// apiError carries the status a call came back with, so a fault can be read
// from it without parsing a message.
type apiError struct {
	status  int
	message string
}

func (a *apiError) Error() string { return fmt.Sprintf("%d: %s", a.status, a.message) }

func faultOf(err error) cloudinv.Fault {
	var api *apiError
	if !errorsAs(err, &api) {
		return cloudinv.FaultUnknown
	}
	switch api.status {
	case http.StatusTooManyRequests:
		return cloudinv.FaultThrottle
	case http.StatusForbidden:
		// Google answers 403 both for a permission the caller lacks and for an
		// API nobody switched on, and only the message separates them.
		if strings.Contains(api.message, "has not been used") || strings.Contains(api.message, "is disabled") {
			return cloudinv.FaultAPIDisabled
		}
		return cloudinv.FaultDenied
	case http.StatusUnauthorized:
		return cloudinv.FaultDenied
	case http.StatusNotFound:
		return cloudinv.FaultNotFound
	}
	return cloudinv.FaultUnknown
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body any, into any) error {
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = strings.TrimSuffix(c.BaseURL, "/") + "/" + strings.TrimPrefix(path, "/")
	}
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("serialising the request body: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	answer, err := client.Do(request)
	if err != nil {
		return err
	}
	defer answer.Body.Close()

	read, err := io.ReadAll(answer.Body)
	if err != nil {
		return err
	}
	if answer.StatusCode >= 300 {
		return &apiError{status: answer.StatusCode, message: messageOf(read)}
	}
	if into == nil || len(read) == 0 {
		return nil
	}
	return json.Unmarshal(read, into)
}

// messageOf reads Google's error envelope, falling back to the raw body.
func messageOf(read []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(read, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return string(read)
}

// settle waits for a long running operation to finish. Most Google deletes and
// creates answer with one, and the shapes agree on "done" and "error".
func (c *Client) settle(ctx context.Context, answer map[string]any) error {
	name, _ := answer["name"].(string)
	done, isOperation := answer["done"].(bool)
	if !isOperation && answer["status"] == nil {
		// Not an operation, so the call already finished.
		return nil
	}

	for attempt := 0; attempt < 60; attempt++ {
		if failure, ok := answer["error"].(map[string]any); ok && failure != nil {
			return fmt.Errorf("the operation failed: %v", failure["message"])
		}
		if done {
			return nil
		}
		if status, ok := answer["status"].(string); ok && status == "DONE" {
			return nil
		}
		if name == "" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}

		var next map[string]any
		if err := c.call(ctx, http.MethodGet, name, nil, nil, &next); err != nil {
			return fmt.Errorf("waiting for %s: %w", name, err)
		}
		answer = next
		done, _ = answer["done"].(bool)
	}
	return fmt.Errorf("the operation did not finish")
}

// expand fills a Discovery path template, which uses {+x} for a value that may
// hold slashes and {x} for one that may not.
func expand(template string, values map[string]string) (string, error) {
	out := template
	for key, value := range values {
		out = strings.ReplaceAll(out, "{+"+key+"}", value)
		out = strings.ReplaceAll(out, "{"+key+"}", url.PathEscape(value))
	}
	if at := strings.Index(out, "{"); at >= 0 {
		return "", fmt.Errorf("the path %s still needs %s", template, out[at:])
	}
	return out, nil
}

// combinations turns a scope into one call per combination of its parameters,
// keyed by something a gap can name.
func combinations(params []string, scope Scope) (map[string]map[string]string, error) {
	out := map[string]map[string]string{"": {}}
	for _, param := range params {
		values := scope[param]
		if len(values) == 0 {
			return nil, fmt.Errorf("the scope names no %s, which this type's list requires", param)
		}
		next := map[string]map[string]string{}
		for key, carried := range out {
			for _, value := range values {
				merged := map[string]string{}
				for k, v := range carried {
					merged[k] = v
				}
				merged[param] = value
				name := value
				if key != "" {
					name = key + "/" + value
				}
				next[name] = merged
			}
		}
		out = next
	}
	return out, nil
}

// single is the one combination a create needs, and refuses an ambiguous scope.
func single(params []string, scope Scope) (map[string]string, error) {
	values := map[string]string{}
	for _, param := range params {
		got := scope[param]
		if len(got) != 1 {
			return nil, fmt.Errorf("a create needs exactly one %s, and the scope names %d", param, len(got))
		}
		values[param] = got[0]
	}
	return values, nil
}

func keysOf(parts map[string]map[string]string) []string {
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// identityAfterCreate finds what to read back. An operation names the resource
// it acted on, and where it does not, the parent and the new name do.
func identityAfterCreate(answer map[string]any, d Descriptor, values map[string]string, spec Spec) string {
	if response, ok := answer["response"].(map[string]any); ok {
		if id, ok := response[d.IdentityField].(string); ok && id != "" {
			return id
		}
	}
	if id, ok := answer[d.IdentityField].(string); ok && id != "" && !strings.Contains(id, "/operations/") {
		return id
	}
	if parent, ok := values["parent"]; ok && spec.Name != "" {
		collection := d.ListPath[strings.LastIndex(d.ListPath, "/")+1:]
		return parent + "/" + collection + "/" + spec.Name
	}
	return ""
}

func errorsAs(err error, target **apiError) bool {
	for err != nil {
		if api, ok := err.(*apiError); ok {
			*target = api
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
