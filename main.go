package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type FieldInfo struct {
	GoName     string
	JSName     string
	GoType     string
	IsPointer  bool
	BaseType   string
	IsBasic    bool
	Kind       FieldKind
	EltType    string
	EltIsBasic bool
	EltIsPtr   bool
	KeyType    string
}

type FieldKind int

const (
	KindBasic FieldKind = iota
	KindStruct
	KindSlice
	KindMap
)

type StructInfo struct {
	Name   string
	Fields []FieldInfo
}

func main() {
	// Custom usage output
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "wago is a Go WebAssembly toolchain helper.")
		fmt.Fprintln(os.Stderr, "\nUsage:")
		fmt.Fprintln(os.Stderr, "  wago [flags]             generates Go WASM wrappers and ES6 JS classes")
		fmt.Fprintln(os.Stderr, "  wago build [arguments]   runs generate, compiles WASM binary, and copies wasm_exec.js")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
	}

	// If the subcommand is build, run it
	if len(os.Args) >= 2 && os.Args[1] == "build" {
		runBuildCommand(os.Args[2:])
		return
	}

	// Otherwise, parse flags for code generation
	typeFlag := flag.String("type", "", "comma-separated list of type names; must be set")
	outputFlag := flag.String("output", "", "output Go file name; default <type>_wago.go")
	jsOutputFlag := flag.String("js-output", "", "output JS file name; default <type>.js")

	flag.Parse()

	if *typeFlag == "" {
		if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
			flag.Usage()
			return
		}
		fmt.Fprintln(os.Stderr, "Error: -type flag is required for code generation, or use 'build' subcommand")
		flag.Usage()
		os.Exit(1)
	}

	runGenCommand(*typeFlag, *outputFlag, *jsOutputFlag)
}

func runGenCommand(typeStr, outputVal, jsOutputVal string) {
	types := strings.Split(typeStr, ",")
	for i := range types {
		types[i] = strings.TrimSpace(types[i])
	}

	dir := "."
	gofile := os.Getenv("GOFILE")
	if gofile != "" {
		dir = filepath.Dir(gofile)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info os.FileInfo) bool {
		name := info.Name()
		return !info.IsDir() &&
			strings.HasSuffix(name, ".go") &&
			!strings.HasSuffix(name, "_test.go") &&
			!strings.HasSuffix(name, "_wago.go")
	}, parser.ParseComments)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing directory %s: %v\n", dir, err)
		os.Exit(1)
	}

	var pkgName string
	structs := make(map[string]*StructInfo)

	for name, pkg := range pkgs {
		pkgName = name
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				typeSpec, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					return true
				}

				structName := typeSpec.Name.Name
				requested := slices.Contains(types, structName)
				if !requested {
					return true
				}

				info := &StructInfo{
					Name: structName,
				}

				for _, field := range structType.Fields.List {
					names := field.Names
					if len(names) == 0 {
						var name string
						switch t := field.Type.(type) {
						case *ast.Ident:
							name = t.Name
						case *ast.StarExpr:
							if ident, ok := t.X.(*ast.Ident); ok {
								name = ident.Name
							}
						}
						if name != "" {
							names = []*ast.Ident{ast.NewIdent(name)}
						}
					}

					for _, nameIdent := range names {
						fInfo := parseField(fset, nameIdent.Name, field.Type)
						info.Fields = append(info.Fields, fInfo)
					}
				}

				structs[structName] = info
				return true
			})
		}
	}

	if pkgName == "" {
		pkgName = "main"
	}

	for _, t := range types {
		if _, ok := structs[t]; !ok {
			fmt.Fprintf(os.Stderr, "Error: type %s not found in package %s\n", t, pkgName)
			os.Exit(1)
		}
	}

	var goOut, jsOut string
	if outputVal != "" {
		goOut = outputVal
	} else if gofile != "" {
		ext := filepath.Ext(gofile)
		base := gofile[:len(gofile)-len(ext)]
		goOut = base + "_wago.go"
	} else {
		goOut = strings.ToLower(types[0]) + "_wago.go"
	}

	if jsOutputVal != "" {
		jsOut = jsOutputVal
	} else if gofile != "" {
		ext := filepath.Ext(gofile)
		base := gofile[:len(gofile)-len(ext)]
		jsOut = base + ".js"
	} else {
		jsOut = strings.ToLower(types[0]) + ".js"
	}

	goCode, err := generateGoCode(pkgName, types, structs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating Go code: %v\n", err)
		os.Exit(1)
	}

	jsCode := generateJSCode(types, structs)

	err = os.WriteFile(goOut, goCode, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing Go file %s: %v\n", goOut, err)
		os.Exit(1)
	}
	fmt.Printf("Generated Go file: %s\n", goOut)

	err = os.WriteFile(jsOut, []byte(jsCode), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing JS file %s: %v\n", jsOut, err)
		os.Exit(1)
	}
	fmt.Printf("Generated JS file: %s\n", jsOut)
}

func runBuildCommand(args []string) {
	buildCmd := flag.NewFlagSet("build", flag.ExitOnError)
	outFlag := buildCmd.String("o", "dist/main.wasm", "output wasm file path")
	buildCmd.Parse(args)

	outputPath := *outFlag
	extraArgs := buildCmd.Args()

	fmt.Println("Running go generate ./...")
	genCmd := exec.Command("go", "generate", "./...")
	genCmd.Stdout = os.Stdout
	genCmd.Stderr = os.Stderr
	if err := genCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go generate failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Building Go WASM binary to: %s...\n", outputPath)

	destDir := filepath.Dir(outputPath)
	if destDir != "" && destDir != "." {
		if err := os.MkdirAll(destDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating destination directory: %v\n", err)
			os.Exit(1)
		}
	}

	cmd := exec.Command("go", append([]string{"build", "-o", outputPath}, extraArgs...)...)
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go build failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Build succeeded!")

	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		out, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error querying GOROOT: %v\n", err)
			os.Exit(1)
		}
		goroot = strings.TrimSpace(string(out))
	}

	wasmExecSrc := filepath.Join(goroot, "lib", "wasm", "wasm_exec.js")
	if _, err := os.Stat(wasmExecSrc); os.IsNotExist(err) {
		wasmExecSrc = filepath.Join(goroot, "misc", "wasm", "wasm_exec.js")
	}
	wasmExecDst := filepath.Join(destDir, "wasm_exec.js")

	fmt.Printf("Copying wasm_exec.js to: %s...\n", wasmExecDst)
	if err := copyFile(wasmExecSrc, wasmExecDst); err != nil {
		fmt.Fprintf(os.Stderr, "Error copying wasm_exec.js: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("wasm_exec.js copied successfully.")

	fmt.Println("Gathering wago-generated JS files...")
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".js") {
			relPath, err := filepath.Rel(".", path)
			if err != nil {
				return nil
			}
			if strings.HasPrefix(filepath.Clean(relPath), filepath.Clean(destDir)) {
				return nil
			}
			content, err := os.ReadFile(path)
			if err == nil && bytes.HasPrefix(content, []byte("// Code generated by wago; DO NOT EDIT.")) {
				targetPath := filepath.Join(destDir, relPath)
				targetDir := filepath.Dir(targetPath)
				if err := os.MkdirAll(targetDir, 0755); err != nil {
					fmt.Fprintf(os.Stderr, "Error creating subdirectory %s: %v\n", targetDir, err)
					return nil
				}
				os.Remove(targetPath)
				if err := os.Rename(path, targetPath); err != nil {
					if err := copyFile(path, targetPath); err == nil {
						os.Remove(path)
					} else {
						fmt.Fprintf(os.Stderr, "Error copying JS file %s to %s: %v\n", path, targetPath, err)
						return nil
					}
				}
				fmt.Printf("Moved generated JS file to destination: %s\n", targetPath)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error gathering generated JS files: %v\n", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func parseField(fset *token.FileSet, fieldName string, expr ast.Expr) FieldInfo {
	info := FieldInfo{
		GoName: fieldName,
		JSName: toCamelCase(fieldName),
	}

	var buf bytes.Buffer
	printer.Fprint(&buf, fset, expr)
	info.GoType = buf.String()

	resolveType(&info, expr)
	return info
}

func resolveType(info *FieldInfo, expr ast.Expr) {
	switch t := expr.(type) {
	case *ast.Ident:
		info.BaseType = t.Name
		info.IsBasic = isBasicType(t.Name)
		if info.IsBasic {
			info.Kind = KindBasic
		} else {
			info.Kind = KindStruct
		}
	case *ast.StarExpr:
		info.IsPointer = true
		resolveType(info, t.X)
	case *ast.ArrayType:
		info.Kind = KindSlice
		var eltBuf bytes.Buffer
		printer.Fprint(&eltBuf, token.NewFileSet(), t.Elt)
		info.EltType = eltBuf.String()

		underlying := t.Elt
		if star, ok := underlying.(*ast.StarExpr); ok {
			info.EltIsPtr = true
			underlying = star.X
		}
		if ident, ok := underlying.(*ast.Ident); ok {
			info.EltIsBasic = isBasicType(ident.Name)
			info.BaseType = ident.Name
		} else {
			info.BaseType = info.EltType
		}
	case *ast.MapType:
		info.Kind = KindMap
		var keyBuf bytes.Buffer
		printer.Fprint(&keyBuf, token.NewFileSet(), t.Key)
		info.KeyType = keyBuf.String()

		var valBuf bytes.Buffer
		printer.Fprint(&valBuf, token.NewFileSet(), t.Value)
		info.EltType = valBuf.String()

		underlying := t.Value
		if star, ok := underlying.(*ast.StarExpr); ok {
			info.EltIsPtr = true
			underlying = star.X
		}
		if ident, ok := underlying.(*ast.Ident); ok {
			info.EltIsBasic = isBasicType(ident.Name)
			info.BaseType = ident.Name
		} else {
			info.BaseType = info.EltType
		}
	default:
		info.BaseType = info.GoType
		info.IsBasic = false
		info.Kind = KindStruct
	}
}

func isBasicType(name string) bool {
	switch name {
	case "string", "bool",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64":
		return true
	}
	return false
}

func toCamelCase(s string) string {
	if len(s) == 0 {
		return ""
	}
	runes := []rune(s)
	if !unicode.IsUpper(runes[0]) {
		return s
	}

	allUpper := !slices.ContainsFunc(runes, unicode.IsLower)
	if allUpper {
		return strings.ToLower(s)
	}

	for i := range runes {
		if i+1 < len(runes) && unicode.IsUpper(runes[i]) && unicode.IsLower(runes[i+1]) {
			if i == 0 {
				runes[0] = unicode.ToLower(runes[0])
			} else {
				for j := 0; j < i; j++ {
					runes[j] = unicode.ToLower(runes[j])
				}
			}
			break
		}
	}
	return string(runes)
}

func generateGoCode(pkgName string, types []string, structs map[string]*StructInfo) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("//go:build js && wasm\n\n")
	buf.WriteString("// Code generated by wago; DO NOT EDIT.\n\n")
	buf.WriteString(fmt.Sprintf("package %s\n\n", pkgName))
	buf.WriteString("import \"syscall/js\"\n\n")

	for _, tName := range types {
		st, ok := structs[tName]
		if !ok {
			continue
		}

		buf.WriteString(fmt.Sprintf("func (u %s) ToJSValue() js.Value {\n", tName))
		buf.WriteString("\tobj := js.Global().Get(\"Object\").New()\n")
		for _, f := range st.Fields {
			switch f.Kind {
			case KindBasic:
				if f.IsPointer {
					buf.WriteString(fmt.Sprintf("\tif u.%s != nil {\n", f.GoName))
					buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", js.ValueOf(*u.%s))\n", f.JSName, f.GoName))
					buf.WriteString("\t} else {\n")
					buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", js.Null())\n", f.JSName))
					buf.WriteString("\t}\n")
				} else {
					buf.WriteString(fmt.Sprintf("\tobj.Set(\"%s\", js.ValueOf(u.%s))\n", f.JSName, f.GoName))
				}
			case KindStruct:
				if f.IsPointer {
					buf.WriteString(fmt.Sprintf("\tif u.%s != nil {\n", f.GoName))
					buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", u.%s.ToJSValue())\n", f.JSName, f.GoName))
					buf.WriteString("\t} else {\n")
					buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", js.Null())\n", f.JSName))
					buf.WriteString("\t}\n")
				} else {
					buf.WriteString(fmt.Sprintf("\tobj.Set(\"%s\", u.%s.ToJSValue())\n", f.JSName, f.GoName))
				}
			case KindSlice:
				buf.WriteString(fmt.Sprintf("\tif u.%s != nil {\n", f.GoName))
				buf.WriteString(fmt.Sprintf("\t\tarr%s := js.Global().Get(\"Array\").New(len(u.%s))\n", f.GoName, f.GoName))
				buf.WriteString(fmt.Sprintf("\t\tfor i, v := range u.%s {\n", f.GoName))
				if f.EltIsBasic {
					if f.EltIsPtr {
						buf.WriteString("\t\t\tif v != nil {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tarr%s.SetIndex(i, js.ValueOf(*v))\n", f.GoName))
						buf.WriteString("\t\t\t} else {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tarr%s.SetIndex(i, js.Null())\n", f.GoName))
						buf.WriteString("\t\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\tarr%s.SetIndex(i, js.ValueOf(v))\n", f.GoName))
					}
				} else {
					if f.EltIsPtr {
						buf.WriteString("\t\t\tif v != nil {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tarr%s.SetIndex(i, v.ToJSValue())\n", f.GoName))
						buf.WriteString("\t\t\t} else {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tarr%s.SetIndex(i, js.Null())\n", f.GoName))
						buf.WriteString("\t\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\tarr%s.SetIndex(i, v.ToJSValue())\n", f.GoName))
					}
				}
				buf.WriteString("\t\t}\n")
				buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", arr%s)\n", f.JSName, f.GoName))
				buf.WriteString("\t} else {\n")
				buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", js.Null())\n", f.JSName))
				buf.WriteString("\t}\n")
			case KindMap:
				buf.WriteString(fmt.Sprintf("\tif u.%s != nil {\n", f.GoName))
				buf.WriteString(fmt.Sprintf("\t\tmap%s := js.Global().Get(\"Object\").New()\n", f.GoName))
				buf.WriteString(fmt.Sprintf("\t\tfor k, v := range u.%s {\n", f.GoName))
				if f.EltIsBasic {
					if f.EltIsPtr {
						buf.WriteString("\t\t\tif v != nil {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tmap%s.Set(k, js.ValueOf(*v))\n", f.GoName))
						buf.WriteString("\t\t\t} else {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tmap%s.Set(k, js.Null())\n", f.GoName))
						buf.WriteString("\t\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\tmap%s.Set(k, js.ValueOf(v))\n", f.GoName))
					}
				} else {
					if f.EltIsPtr {
						buf.WriteString("\t\t\tif v != nil {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tmap%s.Set(k, v.ToJSValue())\n", f.GoName))
						buf.WriteString("\t\t\t} else {\n")
						buf.WriteString(fmt.Sprintf("\t\t\t\tmap%s.Set(k, js.Null())\n", f.GoName))
						buf.WriteString("\t\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\tmap%s.Set(k, v.ToJSValue())\n", f.GoName))
					}
				}
				buf.WriteString("\t\t}\n")
				buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", map%s)\n", f.JSName, f.GoName))
				buf.WriteString("\t} else {\n")
				buf.WriteString(fmt.Sprintf("\t\tobj.Set(\"%s\", js.Null())\n", f.JSName))
				buf.WriteString("\t}\n")
			}
		}
		buf.WriteString("\treturn obj\n")
		buf.WriteString("}\n\n")

		buf.WriteString(fmt.Sprintf("func %sFromJSValue(val js.Value) %s {\n", tName, tName))
		buf.WriteString(fmt.Sprintf("\tvar u %s\n", tName))
		buf.WriteString("\tif val.IsUndefined() || val.IsNull() {\n\t\treturn u\n\t}\n")
		for _, f := range st.Fields {
			buf.WriteString(fmt.Sprintf("\tif temp := val.Get(\"%s\"); !temp.IsUndefined() && !temp.IsNull() {\n", f.JSName))
			switch f.Kind {
			case KindBasic:
				conv := generateGoConv("temp", f.BaseType)
				if f.IsPointer {
					buf.WriteString(fmt.Sprintf("\t\tval%s := %s\n", f.GoName, conv))
					buf.WriteString(fmt.Sprintf("\t\tu.%s = &val%s\n", f.GoName, f.GoName))
				} else {
					buf.WriteString(fmt.Sprintf("\t\tu.%s = %s\n", f.GoName, conv))
				}
			case KindStruct:
				if f.IsPointer {
					buf.WriteString(fmt.Sprintf("\t\tval%s := %sFromJSValue(temp)\n", f.GoName, f.BaseType))
					buf.WriteString(fmt.Sprintf("\t\tu.%s = &val%s\n", f.GoName, f.GoName))
				} else {
					buf.WriteString(fmt.Sprintf("\t\tu.%s = %sFromJSValue(temp)\n", f.GoName, f.BaseType))
				}
			case KindSlice:
				buf.WriteString(fmt.Sprintf("\t\tu.%s = make(%s, temp.Length())\n", f.GoName, f.GoType))
				fmt.Fprintf(&buf, "\t\tfor i := 0; i < temp.Length(); i++ {\n")
				buf.WriteString("\t\t\titem := temp.Index(i)\n")
				buf.WriteString("\t\t\tif !item.IsUndefined() && !item.IsNull() {\n")
				if f.EltIsBasic {
					conv := generateGoConv("item", f.BaseType)
					if f.EltIsPtr {
						buf.WriteString(fmt.Sprintf("\t\t\t\tvalItem := %s\n", conv))
						buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[i] = &valItem\n", f.GoName))
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[i] = %s\n", f.GoName, conv))
					}
				} else {
					if f.EltIsPtr {
						buf.WriteString(fmt.Sprintf("\t\t\t\tvalItem := %sFromJSValue(item)\n", f.BaseType))
						buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[i] = &valItem\n", f.GoName))
					} else {
						buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[i] = %sFromJSValue(item)\n", f.GoName, f.BaseType))
					}
				}
				buf.WriteString("\t\t\t}\n")
				buf.WriteString("\t\t}\n")
			case KindMap:
				if f.KeyType != "string" {
					fmt.Fprintf(os.Stderr, "Warning: map key for field %s is not string, skipping deserialization mapping\n", f.GoName)
				} else {
					buf.WriteString(fmt.Sprintf("\t\tu.%s = make(%s)\n", f.GoName, f.GoType))
					buf.WriteString("\t\tkeys := js.Global().Get(\"Object\").Call(\"keys\", temp)\n")
					buf.WriteString("\t\tfor i := 0; i < keys.Length(); i++ {\n")
					buf.WriteString("\t\t\tk := keys.Index(i).String()\n")
					buf.WriteString("\t\t\titem := temp.Get(k)\n")
					buf.WriteString("\t\t\tif !item.IsUndefined() && !item.IsNull() {\n")
					if f.EltIsBasic {
						conv := generateGoConv("item", f.BaseType)
						if f.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\tvalItem := %s\n", conv))
							buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[k] = &valItem\n", f.GoName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[k] = %s\n", f.GoName, conv))
						}
					} else {
						if f.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\tvalItem := %sFromJSValue(item)\n", f.BaseType))
							buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[k] = &valItem\n", f.GoName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\tu.%s[k] = %sFromJSValue(item)\n", f.GoName, f.BaseType))
						}
					}
					buf.WriteString("\t\t\t}\n")
					buf.WriteString("\t\t}\n")
				}
			}
			buf.WriteString("\t}\n")
		}
		buf.WriteString("\treturn u\n")
		buf.WriteString("}\n\n")
	}

	return format.Source(buf.Bytes())
}

func generateGoConv(varName, baseType string) string {
	switch baseType {
	case "string":
		return fmt.Sprintf("%s.String()", varName)
	case "bool":
		return fmt.Sprintf("%s.Bool()", varName)
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return fmt.Sprintf("%s(%s.Int())", baseType, varName)
	case "float32", "float64":
		return fmt.Sprintf("%s(%s.Float())", baseType, varName)
	default:
		return varName
	}
}

func generateJSCode(types []string, structs map[string]*StructInfo) string {
	var buf bytes.Buffer

	buf.WriteString("// Code generated by wago; DO NOT EDIT.\n\n")

	for _, tName := range types {
		st, ok := structs[tName]
		if !ok {
			continue
		}

		buf.WriteString("/**\n")
		for _, f := range st.Fields {
			jsDocType := getJSDocType(f)
			buf.WriteString(fmt.Sprintf(" * @property {%s} %s\n", jsDocType, f.JSName))
		}
		buf.WriteString(" */\n")

		buf.WriteString(fmt.Sprintf("export class %s {\n", tName))

		buf.WriteString("\t/**\n")
		for _, f := range st.Fields {
			jsDocType := getJSDocType(f)
			buf.WriteString(fmt.Sprintf("\t * @param {%s} %s\n", jsDocType, f.JSName))
		}
		buf.WriteString("\t */\n")

		var params []string
		for _, f := range st.Fields {
			params = append(params, f.JSName)
		}
		buf.WriteString(fmt.Sprintf("\tconstructor(%s) {\n", strings.Join(params, ", ")))
		for _, f := range st.Fields {
			buf.WriteString(fmt.Sprintf("\t\tthis.%s = %s;\n", f.JSName, f.JSName))
		}
		buf.WriteString("\t}\n\n")

		buf.WriteString("\t/**\n")
		buf.WriteString("\t * @param {object} obj\n")
		buf.WriteString(fmt.Sprintf("\t * @returns {%s|null}\n", tName))
		buf.WriteString("\t */\n")
		buf.WriteString("\tstatic fromJS(obj) {\n")
		buf.WriteString("\t\tif (!obj) return null;\n")
		buf.WriteString(fmt.Sprintf("\t\treturn new %s(\n", tName))
		for i, f := range st.Fields {
			comma := ""
			if i < len(st.Fields)-1 {
				comma = ","
			}
			conv := generateJSFromJSConv("obj."+f.JSName, f)
			buf.WriteString(fmt.Sprintf("\t\t\t%s%s\n", conv, comma))
		}
		buf.WriteString("\t\t);\n")
		buf.WriteString("\t}\n\n")

		buf.WriteString("\t/**\n")
		buf.WriteString("\t * @returns {object}\n")
		buf.WriteString("\t */\n")
		buf.WriteString("\ttoJS() {\n")
		buf.WriteString("\t\treturn {\n")
		for i, f := range st.Fields {
			comma := ""
			if i < len(st.Fields)-1 {
				comma = ","
			}
			conv := generateJSToJSConv("this."+f.JSName, f)
			buf.WriteString(fmt.Sprintf("\t\t\t%s: %s%s\n", f.JSName, conv, comma))
		}
		buf.WriteString("\t\t};\n")
		buf.WriteString("\t}\n")

		buf.WriteString("}\n\n")
	}

	return buf.String()
}

func getJSDocType(f FieldInfo) string {
	var base string
	switch f.Kind {
	case KindBasic:
		switch f.BaseType {
		case "string":
			base = "string"
		case "bool":
			base = "boolean"
		default:
			base = "number"
		}
	case KindStruct:
		base = f.BaseType
	case KindSlice:
		var elt string
		if f.EltIsBasic {
			switch f.BaseType {
			case "string":
				elt = "string"
			case "bool":
				elt = "boolean"
			default:
				elt = "number"
			}
		} else {
			elt = f.BaseType
		}
		base = fmt.Sprintf("Array.<%s>", elt)
	case KindMap:
		var elt string
		if f.EltIsBasic {
			switch f.BaseType {
			case "string":
				elt = "string"
			case "bool":
				elt = "boolean"
			default:
				elt = "number"
			}
		} else {
			elt = f.BaseType
		}
		base = fmt.Sprintf("Object.<string, %s>", elt)
	}

	if f.IsPointer || f.EltIsPtr {
		return base + "|null"
	}
	return base
}

func generateJSFromJSConv(varName string, f FieldInfo) string {
	switch f.Kind {
	case KindBasic:
		return varName
	case KindStruct:
		return fmt.Sprintf("%s.fromJS(%s)", f.BaseType, varName)
	case KindSlice:
		if f.EltIsBasic {
			return fmt.Sprintf("%s || []", varName)
		}
		return fmt.Sprintf("%s ? %s.map(item => %s.fromJS(item)) : []", varName, varName, f.BaseType)
	case KindMap:
		if f.EltIsBasic {
			return fmt.Sprintf("%s || {}", varName)
		}
		return fmt.Sprintf("(() => { const res = {}; if (%s) { for (const k in %s) { res[k] = %s.fromJS(%s[k]); } } return res; })()", varName, varName, f.BaseType, varName)
	default:
		return varName
	}
}

func generateJSToJSConv(varName string, f FieldInfo) string {
	switch f.Kind {
	case KindBasic:
		return varName
	case KindStruct:
		if f.IsPointer {
			return fmt.Sprintf("%s ? %s.toJS() : null", varName, varName)
		}
		return fmt.Sprintf("%s.toJS()", varName)
	case KindSlice:
		if f.EltIsBasic {
			return varName
		}
		return fmt.Sprintf("%s ? %s.map(item => item ? item.toJS() : null) : []", varName, varName)
	case KindMap:
		if f.EltIsBasic {
			return varName
		}
		return fmt.Sprintf("(() => { const res = {}; if (%s) { for (const k in %s) { res[k] = %s[k] ? %s[k].toJS() : null; } } return res; })()", varName, varName, varName, varName)
	default:
		return varName
	}
}
