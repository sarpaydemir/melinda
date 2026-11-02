package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// SecretKey is a settings key that holds a secret, declared by the adapter that reads it. A reference in Ref
// (env:NAME or file:/path) gives the value instead, while the key is falsy or, with Overwrite, always.
type SecretKey struct {
	Key       string
	Ref       string // Key + "_secret" when empty
	Overwrite bool
}

// read returns the settings document from the first configured source, with its secret references resolved.
func (p *Provider) read() (map[string]json.RawMessage, error) {
	source := p.env.SettingsSource()
	var raw []byte
	var err error
	switch source {
	case "":
		return nil, errors.New("no settings source: set DOST_SETTINGS_FILE or DOST_SETTINGS_JSON")
	case "DOST_SETTINGS_FILE":
		raw, err = os.ReadFile(p.env.SettingsFile)
	case "DOST_SETTINGS_JSON":
		raw = []byte(p.env.SettingsJSON)
	}
	var doc map[string]json.RawMessage
	if err == nil {
		doc, err = document(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if err := p.resolve(doc); err != nil {
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

// resolve replaces each secret reference in doc with the secret's value.
func (p *Provider) resolve(doc map[string]json.RawMessage) error {
	var errs []error
	for _, k := range p.keys {
		refKey := cmp.Or(k.Ref, k.Key+"_secret")
		ref, _ := textfmt.Decode(doc[refKey], false)
		if !textfmt.Truthy(ref) || (!k.Overwrite && truthy(doc[k.Key])) {
			continue
		}
		name, _ := ref.(string)
		value, err := secret(name)
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

// secret returns the JSON string value of the secret that ref names. Other values are not echoed in the error, as
// they may be the secret itself.
func secret(ref string) (json.RawMessage, error) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v := os.Getenv(name)
		if v == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
		return json.Marshal(v)
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(strings.TrimRight(string(b), "\r\n"))
	}
	return nil, errors.New("not an env:NAME or file:/path reference")
}
