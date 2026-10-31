package config

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

// SecretKey is a settings key that holds a secret, declared by the adapter that reads it. A reference in Ref
// (an environment variable, a file, or a Prefect Secret block) gives the value instead, while the key is falsy or,
// with Overwrite, always.
type SecretKey struct {
	Key       string
	Ref       string // Key + "_secret" when empty
	Overwrite bool
}

// read returns the settings document from the first configured source, with its secret references resolved.
func (p *Provider) read(ctx context.Context) (map[string]json.RawMessage, error) {
	source := p.env.SettingsSource()
	var raw []byte
	var err error
	local := true
	switch source {
	case "":
		return nil, errors.New("no settings source: set DOST_SETTINGS_FILE, DOST_SETTINGS_JSON, or DOST_PREFECT_API_URL")
	case "DOST_SETTINGS_FILE":
		raw, err = os.ReadFile(p.env.SettingsFile)
	case "DOST_SETTINGS_JSON":
		raw = []byte(p.env.SettingsJSON)
	default:
		local = false
		raw, err = p.variable(ctx)
	}
	var doc map[string]json.RawMessage
	if err == nil {
		doc, err = document(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if err := p.resolve(ctx, doc, local); err != nil {
		return nil, err
	}
	for _, k := range p.keys {
		var v string
		if json.Unmarshal(doc[k.Key], &v) == nil {
			p.mask.Add(v)
		}
	}
	return doc, nil
}

// document parses a JSON object, or a JSON string that holds one.
func document(raw []byte) (map[string]json.RawMessage, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = []byte(s)
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return nil, errors.New("not a JSON object or a JSON string holding one")
	}
	return doc, nil
}

// resolve replaces each secret reference in doc with the secret's value. Environment and file references are
// honored only in a document from a local source; otherwise every reference names a Prefect Secret block.
func (p *Provider) resolve(ctx context.Context, doc map[string]json.RawMessage, local bool) error {
	var errs []error
	for _, k := range p.keys {
		refKey := cmp.Or(k.Ref, k.Key+"_secret")
		ref, _ := textfmt.Decode(doc[refKey], false)
		if !textfmt.Truthy(ref) || (!k.Overwrite && truthy(doc[k.Key])) {
			continue
		}
		name, ok := ref.(string)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: expected string", refKey))
			continue
		}
		value, err := p.secret(ctx, name, local)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", refKey, err))
			continue
		}
		doc[k.Key] = value
	}
	return errors.Join(errs...)
}

func truthy(raw json.RawMessage) bool {
	v, err := textfmt.Decode(raw, false)
	return err == nil && textfmt.Truthy(v)
}

// secret returns the JSON value of the secret that ref names: env:NAME, file:/path, or a Prefect Secret block.
func (p *Provider) secret(ctx context.Context, ref string, local bool) (json.RawMessage, error) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok && local {
		v := os.Getenv(name)
		if v == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
		return json.Marshal(v)
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok && local {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(strings.TrimRight(string(b), "\r\n"))
	}
	if p.env.PrefectAPIURL == "" {
		return nil, fmt.Errorf("secret block %q: DOST_PREFECT_API_URL is not set", ref)
	}
	name := strings.TrimPrefix(ref, "secret/")
	var block struct {
		Data struct {
			Value json.RawMessage `json:"value"`
		} `json:"data"`
	}
	found, err := p.prefect(ctx, "/block_types/slug/secret/block_documents/name/"+url.PathEscape(name)+"?include_secrets=true", &block)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("secret block %q not found in Prefect", name)
	}
	return secretText(block.Data.Value), nil
}

// secretText is the text a Prefect Secret block's value reads as: a string holding JSON is decoded once, and a
// value that is not a string then reads in its text form (`True`, `1000.0`, `{'k': 'v'}`). A null value stays null.
func secretText(raw json.RawMessage) json.RawMessage {
	v, _ := textfmt.Decode(raw, true)
	if s, ok := v.(string); ok {
		if decoded, err := textfmt.Decode([]byte(s), true); err == nil {
			v = decoded
		}
	}
	if v == nil {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(textfmt.Str(v)) // a string always marshals
	return b
}

// variable returns the value of the Prefect variable that holds the settings document.
func (p *Provider) variable(ctx context.Context) ([]byte, error) {
	var v struct {
		Value json.RawMessage `json:"value"`
	}
	found, err := p.prefect(ctx, "/variables/name/"+url.PathEscape(p.env.PrefectVariable), &v)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("settings variable not found")
	}
	return v.Value, nil
}

// prefect decodes the JSON answer to GET path on the Prefect API into v. It reports false for 404.
func (p *Provider) prefect(ctx context.Context, path string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.env.PrefectAPIURL, "/")+path, nil)
	if err != nil {
		return false, err
	}
	switch {
	case p.env.PrefectAPIAuthString != "":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.env.PrefectAPIAuthString)))
	case p.env.PrefectAPIKey != "":
		req.Header.Set("Authorization", "Bearer "+p.env.PrefectAPIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("GET %s from Prefect: %s", req.URL.Path, resp.Status)
	}
	if json.NewDecoder(resp.Body).Decode(v) != nil {
		return false, fmt.Errorf("GET %s from Prefect: response is not the expected JSON", req.URL.Path)
	}
	return true, nil
}
