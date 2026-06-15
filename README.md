# wago

`wago` is a developer toolchain helper for Go WebAssembly (WASM). It automates the generation of type-safe conversion bindings and function bindings between Go and JavaScript, modeled similarly to Rust's `wasm-bindgen`.

## Features

- **Annotated Exports**: Expose Go functions and structs directly to JavaScript using `//wago:export` comments.
- **Async Promise Support**: Mark functions with `//wago:export async` to run Go execution inside a goroutine and seamlessly return a JavaScript `Promise` to prevent blocking the JS event loop. Captured Go panics reject the Promise.
- **TypedArray Optimization**: Slices of type `[]byte` and `[]uint8` are passed directly as native `Uint8Array`s using optimized binary memory copying (`js.CopyBytesToGo` / `js.CopyBytesToJS`) rather than element-by-element mapping.
- **Safe Pointer Mapping**: Support for basic pointer types (`*string`, `*int`), struct pointers (`*Address`), pointer slices (`[]*Friend`), and maps containing pointers (`map[string]*int`) with nil/null checks to ensure panic-free serialization and deserialization.
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

type Friend struct {
	Name string
}

type User struct {
	Name       string
	Age        int
	BestFriend *Friend
	AllFriends []*Friend
}

//wago:export
func ProcessUser(u User) string {
	return "Hello " + u.Name
}

//wago:export async
func FetchUserAsync(id int) (User, error) {
	// Runs on a Go goroutine and returns a Promise in JS
	if id < 0 {
		return User{}, fmt.Errorf("invalid ID")
	}
	return User{Name: "Alice", Age: 30}, nil
}

//wago:export
func ProcessBytes(data []byte) []byte {
	// Uses fast TypedArray (Uint8Array) copy
	for i := range data {
		data[i] += 1
	}
	return data
}
```

Because `ProcessUser` is exported and references `User` (which references `Friend`), both structs will be automatically resolved and generated.

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
   └── test_struct.js
   ```

### 3. JavaScript Integration

Import and run the typed ES6 wrapper functions directly in JavaScript:

```javascript
import { ProcessUser, FetchUserAsync, ProcessBytes, User, Friend } from './dist/test_struct.js';

// 1. Synchronous struct mapping
const friend = new Friend("Bob");
const user = new User("Alice", 30, friend, [friend]);
console.log(ProcessUser(user)); // "Hello Alice"

// 2. Async Promise execution
try {
	const resultUser = await FetchUserAsync(42);
	console.log(resultUser.name); // "Alice"
} catch (err) {
	console.error(err);
}

// 3. Fast Uint8Array passing
const data = new Uint8Array([1, 2, 3]);
const modified = ProcessBytes(data);
console.log(modified); // Uint8Array [2, 3, 4]
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
