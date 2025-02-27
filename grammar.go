// package grammar allows defining regexp rules with comments, whitespace and
// newlines to make them less dense, and easier to read:
//
//    `                     // a NUMBER
//     [+-]?                // first, match an optional sign
//     (                    // then match integers or f.p. mantissas:
//         \d+\.\d+         // mantissa of the form a.b
//        |\d+\.            // mantissa of the form a.
//        |\.\d+            // mantissa of the form .b
//        |\d+              // integer of the form a
//     )
//     ( [eE] [+-]? \d+ )?  // finally, optionally match an exponent
//    `
//
// result: [+-]?(\d+\.\d+|\d+\.|\.\d+|\d+)([eE][+-]?\d+)?
//
// Complex rules can be assembled by simpler rules (subrules) using string interpolation.
//
//     `
//      ^
//        ${NUMBER}        // start with number
//        (?:              // don't capture
//          \s+ ${NUMBER}  // followed by one ore more numbers, separated by whitespace
//        )+
//     $
//    `
//
// Any number of rules can be added to a grammar, dependent or independent,
// as long as there are no cyclic dependencies.
//
package grammar

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"text/template"
)

// make rule names type safe, too many strings on the road
type ruleName string

// substitute/interploate ${SUBRULE} with final string.
type replaceMap map[ruleName]string

var (
	// regexps for trim
	rxComment = regexp.MustCompile(`(?m://.*$)`)
	rxSpaces  = regexp.MustCompile(`\s+`)

	// regexps for interpolation
	rxGrepSubRuleRelaxed = regexp.MustCompile(Trim(`\$\{ (?P<SUBRULE> [^{}]+ ) \}`))
	rxMatchSubRuleStrict = regexp.MustCompile(Trim(`^ [a-zA-Z_] \w* $`))
)

// Trim removes all comments and whitespace from string.
//
// Trim is a helper function and would normally not be public,
// but it is also helpful if you don't want to build whole grammars,
// but just want to remove whitespace and comments from patterns.
func Trim(s string) string {
	s = rxComment.ReplaceAllString(s, "")
	s = rxSpaces.ReplaceAllString(s, "")

	return s
}

// Grammar is a container for related and maybe dependent rules.
// Subrules are string interpolated in other rules before compiling to regexp.
type Grammar struct {
	name     string             // give the grammar a name
	rules    map[ruleName]*rule // the map of all rules, the rule name is the key
	compiled bool               // all dependencies are resolved und all rules are compiled
}

// rule is a container for a regexp, based on a raw string, ?trimmed?,
// parsed and interpolated with regexp strings from other rules in same grammar.
type rule struct {
	name     ruleName       // give the rule a name
	pattern  string         // the input, trimmed or unaltered
	final    string         // all subrules interpolated
	subrules []ruleName     // a slice of all ${SUBRULE} the rule depends on
	rx       *regexp.Regexp // the compiled regexp
	compiled bool           // all subrules interpolated and the regexp compiled
}

// New initializes a new grammar.
func New(name string) *Grammar {
	return &Grammar{
		name:  name,
		rules: make(map[ruleName]*rule),
	}
}

// Add rule to grammar, returns error if rule with same name already exists
// or grammar is already compiled. The pattern string gets trimmed.
func (g *Grammar) Add(name string, pattern string) error {
	return g.add(ruleName(name), Trim(pattern))
}

// AddVerbatim is similar to Add, but no trimming takes place.
// Use this method if whitespace is significant.
func (g *Grammar) AddVerbatim(name string, pattern string) error {
	return g.add(ruleName(name), pattern)
}

func (g *Grammar) add(ruleName ruleName, pattern string) error {
	if g.compiled {
		return fmt.Errorf("grammar %q is already compiled, can't add rule %q", g.name, ruleName)
	}

	if _, ok := g.rules[ruleName]; ok {
		return fmt.Errorf("grammar %q, rule with name %q already exists", g.name, ruleName)
	}

	r := &rule{name: ruleName, pattern: pattern}

	r.subrules = findSubrules(r)
	for _, subrule := range r.subrules {
		if !rxMatchSubRuleStrict.MatchString(string(subrule)) {
			return fmt.Errorf("grammar %q, rule %q, wrong subrule name %q", g.name, ruleName, subrule)
		}

		if subrule == r.name {
			return fmt.Errorf("grammar %q, rule %q is self referencing", g.name, ruleName)
		}
	}

	g.rules[ruleName] = r

	return nil
}

// Compile all rules in grammar. Resolve dependencies, interpolate strings and compile all rules to regexp.
func (g *Grammar) Compile() error {
	if g.compiled {
		return fmt.Errorf("grammar %q is already compiled", g.name)
	}

	// for all rules check if subrules exists in grammar
	// also build the topo datastruct (slice of all rules) in this run
	var topo []*rule

	for _, r := range g.rules {
		for _, subrule := range r.subrules {
			if _, ok := g.rules[subrule]; !ok {
				return fmt.Errorf("compiling grammar %q, rule %q depends on missing subrule %q", g.name, r.name, subrule)
			}
		}

		topo = append(topo, r)
	}

	// poor mans toposort algo
	// sort rules by number of dependencies, ascending
	sort.Slice(topo, func(i, j int) bool { return len(topo[i].subrules) < len(topo[j].subrules) })

	// for simplistic cyclic redundancy detection
	loop := 0

OUTER:
	for {
		// all rules compiled, finish
		if len(topo) == 0 {
			g.compiled = true

			return nil
		}

		loop++

		// TODO, make real toposort
		if loop > 10*len(g.rules) {
			remaining := []ruleName{}
			for i := range topo {
				remaining = append(remaining, topo[i].name)
			}
			return fmt.Errorf("grammar %q, (maybe) cyclic dependency in rules: %v", g.name, remaining)
		}

		// unshift rule
		r := topo[0]
		topo = topo[1:]

		// are all subrules for this rule compiled?
		// ... and build replaceMap for template parse & execute
		replace := replaceMap{}
		for _, name := range r.subrules {
			subrule := g.rules[name]
			if !subrule.compiled {
				// nope, push this rule at end of stack
				topo = append(topo, r)

				continue OUTER
			}

			// replace ${SUBRULE} with final string of SUBRULE
			replace[name] = subrule.final
		}

		// and now parse and execute text/template for this rule and compile the pattern to regexp
		if err := compile(r, replace); err != nil {
			return fmt.Errorf("grammar %q, %w", g.name, err)
		}
	}
}

// Rx returns the compiled regexp for named rule or error if rule is not added or not compiled.
func (g *Grammar) Rx(name string) (*regexp.Regexp, error) {
	r, ok := g.rules[ruleName(name)]
	if !ok {
		return nil, fmt.Errorf("grammar %q, rule %q is not added", g.name, name)
	}

	if !g.compiled {
		return nil, fmt.Errorf("grammar %q is not compiled", g.name)
	}

	return r.rx, nil
}

// findSubrules is a helper to find all ${RULENAME} in string and returns the slice of ruleNames or nil.
func findSubrules(r *rule) []ruleName {
	var result []ruleName

	for _, matches := range rxGrepSubRuleRelaxed.FindAllStringSubmatch(r.pattern, -1) {
		for i, captureGroup := range rxGrepSubRuleRelaxed.SubexpNames() {
			// index 0 is always the empty string
			if i == 0 {
				continue
			}

			if captureGroup != "SUBRULE" {
				panic("logic error, unexpected named capture group: " + captureGroup)
			}

			result = append(result, ruleName(matches[i]))
		}
	}

	return result
}

// compile is a sequence of actions:
// parse the pattern as text/template,
// execute (interpolate/substitute) all subrules
// and compile the final string to regexp.
func compile(r *rule, replace replaceMap) error {
	if r.compiled {
		panic("logic error, rule is already compiled")
	}

	t := template.New(string(r.name))

	// just a trick to get rid of .Name in templates
	// map vars to functions, allows ${foo} instead of ${.foo} in template
	fmap := template.FuncMap{}

	// substitute subrule to final string of subrule
	for k, v := range replace {
		v := v
		fmap[string(k)] = func() string { return v }
	}

	// add the substitutions to the templates function map
	t.Funcs(fmap)

	// allow ${foo} in template as action foo instead of {{foo}}
	t = t.Delims("${", "}")

	// stop processing on missing key
	t.Option("missingkey=error")

	t, err := t.Parse(r.pattern)
	if err != nil {
		return fmt.Errorf("parsing rule %q, %w", r.name, err)
	}

	buf := new(bytes.Buffer)

	// here happens the string interpolation ${rulename} with final string of rulename
	err = t.Execute(buf, nil)
	if err != nil {
		return fmt.Errorf("interpolating rule %q, %w", r.name, err)
	}

	r.final = buf.String()

	rx, err := regexp.Compile(r.final)
	if err != nil {
		return fmt.Errorf("regexp compilation of rule %q, %w", r.name, err)
	}

	r.rx = rx
	r.compiled = true

	return nil
}
