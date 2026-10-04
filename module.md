# Static ES modules (`ModuleLoader`)

Goja ships an embedded scripting engine whose traditional extension point is a
CommonJS-style `require` registry. CommonJS copies an exported value at the time
it is read, so it cannot express the **live bindings** of ECMAScript modules.
This document describes the scoped, host-driven static module entry point added
alongside `require`, which implements real ES module semantics without touching
the ordinary script pipeline.

Module source is **always** provided by the embedding application through Go
callbacks. The runtime never reads modules from disk or the network.

## Quick start

```go
vm := goja.New()

loader := goja.NewModuleLoader(
	vm,
	// resolve: map a specifier as written in an import statement to a
	// canonical module ID. May be nil to use the specifier itself as the ID.
	func(referrer, specifier string) (string, error) {
		return specifier, nil
	},
	// load: return the source text for a canonical ID. No disk/network access
	// is performed by goja itself.
	func(id string) (string, error) {
		src, ok := sources[id]
		if !ok {
			return "", goja.ErrModuleNotFound
		}
		return src, nil
	},
)

// One-shot convenience (parse + link/instantiate + evaluate):
_, err := loader.RunModule("main")

// Or drive the phases separately (see "Phases" below):
id, err := loader.ParseModule("main")
id, err = loader.Instantiate("main")
value, err := loader.Evaluate(id)
ns, err := loader.ModuleNamespace(id) // live getters for every export
```

Module scope and global script scope share the same `*Runtime`, so modules can
call host functions installed with `vm.Set(...)`, and ordinary scripts run with
`vm.RunScript` are completely unaffected by the presence of modules.

## Canonical IDs and aliases

Only the value returned by the `resolve` callback identifies a module. Two
different specifiers that resolve to the same canonical ID are aliases for one
module: they share a single module record, a single set of live bindings, and a
single evaluation.

```go
resolve := func(referrer, spec string) (string, error) {
	if spec == "alias-a" || spec == "alias-b" {
		return "the-real-module", nil
	}
	return spec, nil
}
```

If `resolve` is `nil`, the raw specifier string is the canonical ID.

## Phases

The three specification phases are independently available:

1. **Parsing** — `ParseModule(entrySpecifier) (canonicalID, error)`.
   Recursively invokes `resolve` and `load` and parses every reachable module
   with the module grammar. No module code runs. Returns the canonical entry ID.
2. **Instantiation (linking)** — `Instantiate(entrySpecifier)` (parses first if
   needed). Creates export cells and import views, validates that every
   imported name exists, wires live accessors (including re-exports), and
   compiles module bodies. Still no module code runs.
3. **Evaluation** — `Evaluate(canonicalID)`. Runs dependency bodies in
   depth-first post-order, then the entry body. Each module body runs **at most
   once** per loader. A module that throws is permanently marked errored:
   reaching it again (even through a different import order or entry module)
   throws the cached failure rather than silently re-running or succeeding.

`RunModule` is a thin wrapper over all three.

## Semantics

### Live bindings

Exports are accessor-backed cells, not snapshots. An importer's binding always
reflects the exporter's current value:

```js
// counter.js
export let count = 0;
export function inc() { count++; }

// reader.js
import { count, inc } from "./counter.js";
inc();
count; // 1, seen live through the import
```

`export { x as y }` (with or without `from`) exposes the same underlying
binding, so reassigning the local changes every aliased view.

### Temporal dead zone (TDZ)

`let`, `const` and `class` exports are in the TDZ until their initialising
statement runs. Reading one through a circular import before it is initialised
throws a `ReferenceError` ("Cannot access 'x' before initialization"). Hoisted
`function` declarations and `var` exports are initialised during instantiation,
so they are visible to circular importers before module bodies evaluate.

### Read-only imports and `const`

Imported bindings have no setter; assigning to one in strict module code throws
a `TypeError`. A `const` (or `export default <expression>`) export accepts
exactly one write — its own initialiser — and rejects every later assignment.

### `this`

Top-level `this` in a module is `undefined`.

### Module namespace

`ModuleNamespace(id)` returns an object with one live getter per exported name
(including `default` and re-exported names). Read it from Go with `ns.Get(name)`
or pass it into scripts/modules.

## First-version syntax boundary

The grammar is deliberately small. Anything outside the supported forms is
**rejected before execution**, either as a parse error
(`*goja.ModuleSyntaxError`) or, when it depends on another module's exports, as
a `*goja.LinkError` during instantiation.

Supported:

| Form | Example |
| --- | --- |
| Named imports | `import { a, b as c } from "m";` |
| Default import | `import d from "m";` |
| Combined | `import d, { a } from "m";` |
| Side-effect import | `import "m";` |
| `var` / `let` / `const` export | `export let a = 1, b = 2;` |
| Function export | `export function f() {}`, `export async function g() {}`, generators |
| Class export | `export class C {}` |
| Named clause export | `export { a, b as c };` |
| Explicit re-export | `export { a, b as c } from "m";` |
| Default re-export | `export { default as d } from "m";` |
| Default expression | `export default compute();` |
| Default named/anonymous function or class | `export default function f() {}` / `export default class {}` |

Explicitly **not supported** and always rejected up front:

- Dynamic `import(...)`.
- `import.meta` (including `import.meta.url`).
- Top-level `await`. (`await` inside an `async` function is fine.)
- Namespace imports/re-exports: `import * as ns from "m"`.
- `export * from "m"` (namespace/star re-export).
- Destructuring directly in an export declaration:
  `export const { a } = obj;` / `export const [a] = arr;`
  (Destructuring in non-exported declarations is unaffected.)
- Non-string module specifiers (`import x from 42;`).
- Import attributes / assertions (`with { type: "json" }`).
- String-literal binding names in clauses (`import { "x" as y }`).

Static integrity errors also rejected before execution include: importing a
name the target does not export, declaring the same import local twice, an
import local colliding with a module-scope declaration, duplicate `default`,
and duplicate public export names.

Ordinary scripts parsed with `Compile`/`RunScript`/`RunString` never see the
module grammar — `import`/`export` remain reserved words and produce a syntax
error there, exactly as before.

## API reference

- `func NewModuleLoader(rt *Runtime, resolve ModuleResolveFunc, load ModuleLoadFunc) *ModuleLoader`
- `type ModuleResolveFunc func(referrer, specifier string) (canonicalID string, err error)`
- `type ModuleLoadFunc func(canonicalID string) (src string, err error)`
- `func (*ModuleLoader) ParseModule(entrySpecifier string) (canonicalID string, err error)`
- `func (*ModuleLoader) Instantiate(entrySpecifier string) (canonicalID string, err error)`
- `func (*ModuleLoader) Evaluate(canonicalID string) (Value, error)`
- `func (*ModuleLoader) RunModule(entrySpecifier string) (Value, error)`
- `func (*ModuleLoader) ModuleNamespace(canonicalID string) (*Object, error)`
- `var ErrModuleNotFound` — sentinel a load callback may return.
- `type ModuleSyntaxError` — wraps a parse error with the module ID.
- `type LinkError` — instantiation-time failure with the module ID and message.
- `func parser.ParseModule(fileSet *file.FileSet, filename string, src interface{}, options ...parser.Option) (*ast.Program, error)`
  for callers that only want the module AST.
