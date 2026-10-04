# ES Modules (v1)

Goja v1 modules add a scoped, static module entry point alongside the
existing CommonJS-style script execution. Unlike copying an object, module
bindings are *live*: an importer always observes the exporter's current
value, and bindings are created before any code runs so circular imports
work within the usual temporal-dead-zone rules.

Module source code is only ever obtained from a host callback. The runtime
never reads the file system or the network.

## Host API

```go
type ModuleSourceLoader func(referrerCanonical, specifier string) (canonicalID, source string, err error)

runner := rt.NewModuleRunner(loader)

// Three explicit phases:
resolved, err := runner.Link(entrySpecifier)   // parse + instantiate, no execution
namespace, err := resolved.Evaluate()          // evaluate, at most once per module

// or the convenience wrapper:
namespace, err := runner.RunModule(entrySpecifier)
```

- The loader receives the importing module's canonical ID (`""` for the
  entry) and the raw specifier written after `from`. It returns the source
  and the **canonical ID** of the resolved module.
- Two different specifiers (aliases) that resolve to the same canonical ID
  share a single parsed, instantiated and evaluated module instance.
- `Runtime.CompileModule(name, src)` parses and compiles one module in
  isolation, without resolving imports.

## Phases

1. **Parse** - every module in the graph is fetched through the loader and
   compiled. Syntax errors (including all unsupported forms below) are
   reported here, before anything executes.
2. **Link / instantiate** - environments are created and import bindings
   are wired to the exporter's live cell. A missing export, resolution
   failure or loader error aborts before evaluation.
3. **Evaluate** - module bodies run in dependency order. Each module body
   runs at most once. A module that throws a JavaScript exception enters a
   sticky *errored* state; reaching it again through any import order
   reports the same failure instead of re-running it.

## Binding semantics

- Imports are read-only: assigning to an imported binding is rejected at
  compile time.
- Exported `let`/`var` bindings are mutable and observed live by importers;
  `const` and `default` initialisers are immutable after initialisation.
- Reading a binding before its initialiser has run throws a
  `ReferenceError` (the temporal dead zone), including across cycles.
- Module code always runs in strict mode.

## Supported syntax (v1)

```js
import defaultName from "mod";
import { a, b as c } from "mod";
import defaultName, { a, b as c } from "mod";
import { default as d } from "mod";
import "mod";                                   // side-effect only

export var/let/const declaration;
export function name() {}
export class name {}
export { a, b as c };
export { a, b as c } from "mod";                // explicit re-export
export default <expression>;
export default function [name]() {}
export default class [name] {}
```

## Rejected before execution (v1 boundaries)

The following are parse-time `SyntaxError`s; they never reach link or
evaluation:

- Dynamic `import(...)`.
- Top-level `await`.
- `export *` (and `export * as ns`).
- `import.meta`.
- Namespace imports (`import * as ns`).
- `with` statements (always illegal in strict module code).

Ordinary scripts parsed with `RunString`/`RunScript`/`Compile` are
unaffected: module syntax is only accepted through `ParseModule` /
`CompileModule` / the module runner.
