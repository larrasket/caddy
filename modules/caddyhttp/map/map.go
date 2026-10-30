// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package maphandler

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements a middleware that maps inputs to outputs. Specifically, it
// compares a source value against the map inputs, and for one that matches, it
// applies the output values to each destination. Destinations become placeholder
// names.
//
// Mapped placeholders are not evaluated until they are used, so even for very
// large mappings, this handler is quite efficient.
type Handler struct {
	// Source is the placeholder from which to get the input value.
	Source string `json:"source,omitempty"`

	// Destinations are the names of placeholders in which to store the outputs.
	// Destination values should be wrapped in braces, for example, {my_placeholder}.
	Destinations []string `json:"destinations,omitempty"`

	// Mappings from source values (inputs) to destination values (outputs).
	// The first matching, non-nil mapping will be applied.
	Mappings []Mapping `json:"mappings,omitempty"`

	// If no mappings match or if the mapped output is null/nil, the associated
	// default output will be applied (optional).
	Defaults []string `json:"defaults,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.map",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up h.
func (h *Handler) Provision(_ caddy.Context) error {
	for j, dest := range h.Destinations {
		if !strings.HasPrefix(dest, "{") || !strings.HasSuffix(dest, "}") ||
			strings.Count(dest, "{") != 1 || strings.Count(dest, "}") != 1 ||
			len(dest) < 3 {
			return fmt.Errorf("destination %d must be a placeholder and only a placeholder, but got '%s'", j, dest)
		}
		h.Destinations[j] = dest[1 : len(dest)-1]
	}

	for i, m := range h.Mappings {
		if m.InputRegexp == "" {
			continue
		}
		var err error
		h.Mappings[i].re, err = regexp.Compile(m.InputRegexp)
		if err != nil {
			return fmt.Errorf("compiling regexp for mapping %d: %v", i, err)
		}
	}

	// TODO: improve efficiency even further by using an actual map type
	// for the non-regexp mappings, OR sort them and do a binary search

	return nil
}

// Validate ensures that h is configured properly.
func (h *Handler) Validate() error {
	nDest, nDef := len(h.Destinations), len(h.Defaults)
	if nDef > 0 && nDef != nDest {
		return fmt.Errorf("%d destinations != %d defaults", nDest, nDef)
	}

	seenLiterals := make(map[string]int)
	seenRegexps := make(map[string]int)
	for i, m := range h.Mappings {
		// prevent confusing/ambiguous mappings
		if m.Input != "" && m.InputRegexp != "" {
			return fmt.Errorf("mapping %d has both input and input_regexp fields specified, which is confusing", i)
		}

		// prevent duplicate mappings
		input, seen := m.Input, seenLiterals
		if m.InputRegexp != "" {
			input, seen = m.InputRegexp, seenRegexps
		}
		if prev, ok := seen[input]; ok {
			return fmt.Errorf("mapping %d has a duplicate input '%s' previously used with mapping %d", i, input, prev)
		}
		seen[input] = i

		// ensure mappings have 1:1 output-to-destination correspondence
		nOut := len(m.Outputs)
		if nOut != nDest {
			return fmt.Errorf("mapping %d has %d outputs but there are %d destinations defined", i, nOut, nDest)
		}
	}

	return nil
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// defer work until a variable is actually evaluated by using replacer's Map callback
	repl.Map(func(key string) (any, bool) {
		// return early if the variable is not even a configured destination
		destIdx := slices.Index(h.Destinations, key)
		if destIdx < 0 {
			return nil, false
		}

		input := repl.ReplaceAll(h.Source, "")

		// find the first mapping matching the input and return
		// the requested destination/output value
		for _, m := range h.Mappings {
			output := m.Outputs[destIdx]
			if output == nil {
				continue
			}
			outputStr := caddy.ToString(output)

			// evaluate regular expression if configured
			if m.re != nil {
				matches := m.re.FindStringSubmatchIndex(input)
				if matches == nil {
					continue
				}
				return expandRegexpOutput(repl, m.re, outputStr, input, matches), true
			}

			// otherwise simple string comparison
			if input == m.Input {
				return repl.ReplaceAll(outputStr, ""), true
			}
		}

		// fall back to default if no match or if matched nil value
		if len(h.Defaults) > destIdx {
			return repl.ReplaceAll(h.Defaults[destIdx], ""), true
		}

		return nil, true
	})

	return next.ServeHTTP(w, r)
}

// captureRef matches what regexp.Expand treats specially in a template:
// "$$", "${name}", "$name", and a lone '$', which is literal.
var captureRef = regexp.MustCompile(`\$(\$|\{[\p{L}\p{Nd}_]+\}|[\p{L}\p{Nd}_]+)?`)

// expandRegexpOutput fills in the output of a regexp mapping. Configured
// placeholders are replaced before the captures are inserted, so captured
// input is never evaluated as a placeholder. Capture references are braced
// (with the braces escaped from the replacer) so placeholder values cannot
// merge into them, a lone '$' is kept literal, and '$' in placeholder values
// is escaped so a value is never read as a capture reference.
func expandRegexpOutput(repl *caddy.Replacer, re *regexp.Regexp, output, input string, matches []int) string {
	if !strings.Contains(output, "$") {
		return repl.ReplaceAll(output, "")
	}
	var tmpl strings.Builder
	last := 0
	for _, loc := range captureRef.FindAllStringIndex(output, -1) {
		tmpl.WriteString(output[last:loc[0]])
		ref, rest := output[loc[0]:loc[1]], output[loc[1]:]
		last = loc[1]
		switch name := strings.Trim(ref[1:], "{}"); {
		case ref == "$$":
			tmpl.WriteString(ref)
		case name == "":
			tmpl.WriteString("$$")
		case rest == "" || rest[0] == '}':
			// Nothing can merge into the reference here, and a '}' may end a
			// placeholder name such as {http.request.uri.query.$filter}.
			tmpl.WriteString("$" + name)
		default:
			tmpl.WriteString(`$\{` + name + `\}`)
		}
	}
	tmpl.WriteString(output[last:])
	template, _ := repl.ReplaceFunc(tmpl.String(), func(_ string, val any) (any, error) {
		return strings.ReplaceAll(caddy.ToString(val), "$", "$$"), nil
	})
	return string(re.ExpandString(nil, template, input, matches))
}

// Mapping describes a mapping from input to outputs.
type Mapping struct {
	// The input value to match. Must be distinct from other mappings.
	// Mutually exclusive to input_regexp.
	Input string `json:"input,omitempty"`

	// The input regular expression to match. Mutually exclusive to input.
	InputRegexp string `json:"input_regexp,omitempty"`

	// Upon a match with the input, each output is positionally correlated
	// with each destination of the parent handler. An output that is null
	// (nil) will be treated as if it was not mapped at all.
	Outputs []any `json:"outputs,omitempty"`

	re *regexp.Regexp
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
