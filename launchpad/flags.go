package main

// Translation of the TCK canonical flag set into a GO Feature Flag flag file -- the file the relay
// proxy's file retriever reads.
//
// Schema verified against thomaspoignant/go-feature-flag@v1.57.0 modules/core/flag
// (InternalFlag, Rule) and against the relay proxy's own POST /v1/flag/configuration output.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type canonicalFile struct {
	Flags map[string]canonicalFlag `json:"flags"`
}

type canonicalFlag struct {
	State          string                     `json:"state"`
	Variants       map[string]json.RawMessage `json:"variants"`
	DefaultVariant string                     `json:"defaultVariant"`
	Targeting      json.RawMessage            `json:"targeting,omitempty"`
}

type goffRule struct {
	Name      string `json:"name,omitempty"`
	Query     string `json:"query,omitempty"`
	Variation string `json:"variation"`
}

// goffFlag is one entry of the flag file.
//
// Variation values are kept as json.RawMessage so the generator writes the canonical literals
// untouched. That does not make `10.0` survive: GOFF decodes the file into `any`, so it is
// float64(10) inside the relay proxy and `10` on every wire it serves (OFREP and
// /v1/flag/configuration alike). See FINDINGS.md #4.
type goffFlag struct {
	Variations  map[string]json.RawMessage `json:"variations"`
	Targeting   []goffRule                 `json:"targeting,omitempty"`
	DefaultRule goffRule                   `json:"defaultRule"`
	Disable     bool                       `json:"disable,omitempty"`
}

type flagSet map[string]*goffFlag

// buildFlagSet turns a canonical (flagd-format) flag set into GOFF flags.
//
//   - variants -> variations, defaultVariant -> defaultRule.variation. Variant names carry over
//     unchanged, so the expected variant names in the feature files hold.
//   - state DISABLED -> `disable: true`.
//   - The one targeting shape the canonical set uses -> a single rule with a nikunjy/rules query.
//   - Nothing else gets a rule, so GOFF resolves reason STATIC for every other flag
//     (selectEvaluationReason in modules/core/flag/internal_flag.go).
func buildFlagSet(canonicalPath string) (flagSet, error) {
	b, err := os.ReadFile(canonicalPath)
	if err != nil {
		return nil, err
	}
	var cf canonicalFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", canonicalPath, err)
	}

	set := make(flagSet, len(cf.Flags))
	for name, cflag := range cf.Flags {
		if _, ok := cflag.Variants[cflag.DefaultVariant]; !ok {
			return nil, fmt.Errorf("flag %q: defaultVariant %q not in variants", name, cflag.DefaultVariant)
		}
		f := &goffFlag{
			Variations:  cflag.Variants,
			DefaultRule: goffRule{Variation: cflag.DefaultVariant},
		}
		switch strings.ToUpper(cflag.State) {
		case "ENABLED":
		case "DISABLED":
			f.Disable = true
		default:
			return nil, fmt.Errorf("flag %q: unknown state %q", name, cflag.State)
		}

		if len(cflag.Targeting) > 0 {
			key, variant, err := targetingKeyRule(cflag.Targeting)
			if err != nil {
				return nil, fmt.Errorf("flag %q: %w", name, err)
			}
			if _, ok := cflag.Variants[variant]; !ok {
				return nil, fmt.Errorf("flag %q: targeted variant %q not in variants", name, variant)
			}
			f.Targeting = []goffRule{{
				Name:      "targeting-key",
				Query:     fmt.Sprintf("targetingKey eq %q", key),
				Variation: variant,
			}}
		}
		set[name] = f
	}
	return set, nil
}

// targetingKeyRule is the one targeting shape the canonical set uses:
//
//	{"if": [{"==": [{"var": "targetingKey"}, "<uuid>"]}, "<variant>", null]}
//
// Parsing it narrowly rather than implementing JsonLogic is deliberate, and the canonical set
// licenses it: the rule "is specified by its behaviour, not by this encoding". GOFF expresses it
// natively as a rule `query: targetingKey eq "<uuid>"`; the null else-arm is GOFF's defaultRule.
//
// A shape this does not recognise is an error rather than a silent skip. Seeding a targeting flag
// with no rule would make the "non-matching context" scenario pass for the wrong reason.
func targetingKeyRule(raw json.RawMessage) (key, variant string, err error) {
	var rule struct {
		If []json.RawMessage `json:"if"`
	}
	if err := json.Unmarshal(raw, &rule); err != nil {
		return "", "", fmt.Errorf("targeting is not an {\"if\": [...]} rule: %w", err)
	}
	if len(rule.If) != 3 {
		return "", "", fmt.Errorf("targeting \"if\" has %d arms, want 3", len(rule.If))
	}
	if strings.TrimSpace(string(rule.If[2])) != "null" {
		return "", "", fmt.Errorf("targeting else-arm is not null; only a fall-through to defaultVariant maps onto defaultRule")
	}

	// Decoded into a map because the operator key is literally "==", which no struct tag spells.
	var condition map[string][]json.RawMessage
	if err := json.Unmarshal(rule.If[0], &condition); err != nil {
		return "", "", fmt.Errorf("targeting condition is not an object: %w", err)
	}
	eq, ok := condition["=="]
	if !ok || len(eq) != 2 {
		return "", "", fmt.Errorf("targeting condition is not an \"==\" over two operands")
	}
	var varRef struct {
		Var string `json:"var"`
	}
	if err := json.Unmarshal(eq[0], &varRef); err != nil || varRef.Var != "targetingKey" {
		return "", "", fmt.Errorf("targeting condition does not compare \"targetingKey\"")
	}
	if err := json.Unmarshal(eq[1], &key); err != nil {
		return "", "", fmt.Errorf("targeting condition's operand is not a string: %w", err)
	}
	if strings.ContainsAny(key, `"\`) {
		return "", "", fmt.Errorf("targeting key %q cannot be quoted in a GOFF query", key)
	}
	if err := json.Unmarshal(rule.If[1], &variant); err != nil {
		return "", "", fmt.Errorf("targeting consequent is not a variant name: %w", err)
	}
	return key, variant, nil
}

// defaultValue is the value a flag resolves to with no targeting match.
func (s flagSet) defaultValue(name string) (json.RawMessage, bool) {
	f, ok := s[name]
	if !ok {
		return nil, false
	}
	v, ok := f.Variations[f.DefaultRule.Variation]
	return v, ok
}

func (s flagSet) clone() flagSet {
	b, _ := json.Marshal(s)
	var c flagSet
	_ = json.Unmarshal(b, &c)
	return c
}

// write replaces the flag file atomically, so the retriever never reads a half-written file. A
// torn read would not corrupt anything -- GOFF keeps serving the last good set when a refresh
// fails -- but it would turn a refresh into a silent no-op, and /change would then time out.
func (s flagSet) write(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".flags-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // CreateTemp makes it 0600
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
