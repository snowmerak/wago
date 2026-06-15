# wago

`wago` is a developer toolchain helper for Go WebAssembly (WASM). It automates the generation of type-safe conversion bindings and function bindings between Go and JavaScript, modeled similarly to Rust's `wasm-bindgen`.

## Features

- **Annotated Exports**: Expose Go functions and structs directly to JavaScript using `//wago:export` comments.
- **Transitive Dependency Resolution**: Any struct referenced by an exported function (in parameters/returns) or nested in another exported struct is automatically resolved and generated without manual configuration.
- **Type-safe JS Wrappers**: Generates ES6 JS classes and wrapper functions with complete JSDoc annotations for IDE autocompletion.
- **WASM Build Integration**: `wago build` runs code generation, compiles the binary targeting WebAssembly (`GOOS=js GOARCH=wasm`), and stages compiled binaries, runtime libraries (`wasm_exec.js`), and JS modules under the build folder.
- **Package Layout Preservation**: Staged JS assets mirror the original Go package directory structure under the output folder.

## Installation

```bash
go install wago@latest
```

Or build from source:

```bash
go build -o wago main.go
```

## Usage

### 1. Annotate Go Code

Add `//wago:export` comments to functions or structs that you want to expose:

```go
package main

//go:generate wago

type Profile struct {
	Bio string
}

type User struct {
	Name    string
	Age     int
	Profile Profile
}

//wago:export
func GreetUser(u User) string {
	return "Hello " + u.Name
}
```

Because `GreetUser` is exported and references `User` (which references `Profile`), both structs will be automatically resolved and generated.

### 2. Register Exports in main()

Call the generated `RegisterWagoExports()` function inside your `main()` entrypoint and block the thread to keep the WASM instance alive:

```go
package main

import "syscall/js"

func main() {
	keepAlive := make(chan struct{})

	// Register all annotated wago functions to JS globals
	RegisterWagoExports()

	<-keepAlive
}
```

### 3. Build the Project

Run the unified compiler:

```bash
wago build [-o dist/main.wasm] [extra go build args...]
```

By default, output files are generated and staged under the `dist/` directory:

```
dist/
├── main.wasm
├── wasm_exec.js
└── user_generated.js   (Contains the ES6 class and exported function wrappers)
```

### 4. JavaScript Integration

Import and run the typed ES6 wrapper functions directly in JavaScript:

```javascript
import { GreetUser, User, Profile } from './dist/user_generated.js';

// Instantiate classes
const profile = new Profile("Go WASM Developer");
const user = new User("Alice", 30, profile);

// Call Go WASM function seamlessly
const greeting = GreetUser(user);
console.log(greeting); // "Hello Alice"
```

## How It Works

### Generated Go Bindings (`*_wago.go`)

For annotated functions, `wago` generates `RegisterWagoExports()` using `syscall/js`:

```go
func RegisterWagoExports() {
	js.Global().Set("greetUser", js.FuncOf(func(this js.Value, args []js.Value) any {
		arg_u := UserFromJSValue(args[0])
		res := GreetUser(arg_u)
		return js.ValueOf(res)
	}))
}
```

### Generated JS Wrappers (`*.js`)

For the JS frontend, `wago` generates ES6 classes and function wrappers:

```javascript
export class User {
	// ... constructor and toJS/fromJS helpers
}

/**
 * @param {User} u
 * @returns {string}
 */
export function greetUser(u) {
	const raw_u = u ? u.toJS() : null;
	const res = globalThis.greetUser(raw_u);
	return res;
}
```
