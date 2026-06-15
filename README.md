# wago

`wago` is a developer toolchain helper for Go WebAssembly (WASM). It automates the generation of type-safe conversion bindings and function bindings between Go and JavaScript, modeled similarly to Rust's `wasm-bindgen`.

## Features

- **Annotated Exports**: Expose Go functions and structs directly to JavaScript using `//wago:export` comments.
- **Transitive Dependency Resolution**: Any struct referenced by an exported function (in parameters/returns) or nested in another exported struct is automatically resolved and generated without manual configuration.
- **Auto-generated Entrypoint (`main()`)**: If `wago` detects `package main` but no user-defined `func main()` is present, it automatically generates a `main_wago.go` file containing the Go WASM keep-alive block and exports registration.
- **Type-safe JS Wrappers**: Generates ES6 JS classes and wrapper functions with complete JSDoc annotations for JS autocompletion.
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

Add `//wago:export` comments to functions or structs that you want to expose. You do not need to write a `main()` function:

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

### 2. Build the Project

Run the unified compiler:

```bash
wago build [-o dist/main.wasm] [extra go build args...]
```

When building:
1. `wago` will run `go generate ./...` which triggers generation of Go mappers and JS files.
2. If the package name is `main` and you have not written a `func main()`, `wago` will automatically generate `main_wago.go` containing:
   ```go
   //go:build js && wasm
   package main

   func main() {
       keepAlive := make(chan struct{})
       RegisterWagoExports()
       <-keepAlive
   }
   ```
3. The WebAssembly binary compiles successfully to `dist/main.wasm`.
4. `wasm_exec.js` is copied and all generated JS modules are staged matching the package layout:
   ```
   dist/
   ├── main.wasm
   ├── wasm_exec.js
   └── user_generated.js
   ```

### 3. JavaScript Integration

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

## Custom main() Initialization (Optional)

If you need custom initialization code (e.g. setting up databases or state in Go before exposing functions), you can write your own `func main()` in Go.

If `wago` detects your `main` function, it will **not** generate `main_wago.go`. You must call `RegisterWagoExports()` manually:

```go
package main

import "syscall/js"

func main() {
	keepAlive := make(chan struct{})

	// Perform custom setup here...
	println("Initializing Go WASM module...")

	// Register all annotated wago functions to JS globals
	RegisterWagoExports()

	<-keepAlive
}
```
