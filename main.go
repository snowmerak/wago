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
	"strings"
	"unicode"
)

type FieldInfo struct {
	GoName      string
	JSName      string
	GoType      string
	IsPointer   bool
	BaseType    string
	IsBasic     bool
	Kind        FieldKind
	EltType     string
	EltIsBasic  bool
	EltIsPtr    bool
	KeyType     string
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

type ExportedFunc struct {
	GoName   string
	JSName   string
	Params   []FieldInfo
	Results  []FieldInfo
	HasError bool
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "wago is a Go WebAssembly toolchain helper.")
		fmt.Fprintln(os.Stderr, "\nUsage:")
		fmt.Fprintln(os.Stderr, "  wago [flags]             generates Go WASM wrappers and ES6 JS classes")
		fmt.Fprintln(os.Stderr, "  wago build [arguments]   runs generate, compiles WASM binary, and copies wasm_exec.js")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
	}

	if len(os.Args) >= 2 && os.Args[1] == "build" {
		runBuildCommand(os.Args[2:])
		return
	}

	typeFlag := flag.String("type", "", "comma-separated list of type names; optional if wago:export comments are used")
	outputFlag := flag.String("output", "", "output Go file name; default <type>_wago.go")
	jsOutputFlag := flag.String("js-output", "", "output JS file name; default <type>.js")

	flag.Parse()

	// If no type flag and no help, we will attempt comment-based detection
	if *typeFlag == "" && len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		flag.Usage()
		return
	}

	runGenCommand(*typeFlag, *outputFlag, *jsOutputFlag)
}

func parseWagoExport(doc *ast.CommentGroup) (bool, string) {
	if doc == nil {
		return false, ""
	}
	for _, c := range doc.List {
		text := strings.TrimSpace(c.Text)
		if strings.HasPrefix(text, "//") {
			text = strings.TrimSpace(text[2:])
			if strings.HasPrefix(text, "wago:export") {
				rem := strings.TrimSpace(text[len("wago:export"):])
				return true, rem
			}
		} else if strings.HasPrefix(text, "/*") {
			text = text[2 : len(text)-2]
			text = strings.TrimSpace(text)
			if strings.HasPrefix(text, "wago:export") {
				rem := strings.TrimSpace(text[len("wago:export"):])
				return true, rem
			}
		}
	}
	return false, ""
}

func runGenCommand(typeStr, outputVal, jsOutputVal string) {
	cliTypes := strings.Split(typeStr, ",")
	for i := range cliTypes {
		cliTypes[i] = strings.TrimSpace(cliTypes[i])
	}
	if len(cliTypes) == 1 && cliTypes[0] == "" {
		cliTypes = []string{}
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
	allStructs := make(map[string]*StructInfo)
	annotatedStructs := make(map[string]bool)
	var exportedFuncs []ExportedFunc

	hasUserMain := false
	for name, pkg := range pkgs {
		pkgName = name
		for _, file := range pkg.Files {
			// Find annotated functions and structs
			for _, decl := range file.Decls {
				if funcDecl, ok := decl.(*ast.FuncDecl); ok {
					if funcDecl.Recv == nil && funcDecl.Name.Name == "main" {
						hasUserMain = true
					}
					if ok, customName := parseWagoExport(funcDecl.Doc); ok {
						jsName := customName
						if jsName == "" {
							jsName = toCamelCase(funcDecl.Name.Name)
						}

						params := parseFuncParams(fset, funcDecl.Type.Params.List)
						var results []FieldInfo
						if funcDecl.Type.Results != nil {
							results = parseFuncParams(fset, funcDecl.Type.Results.List)
						}

						hasError := false
						if len(results) > 0 {
							lastResult := results[len(results)-1]
							if lastResult.GoType == "error" {
								hasError = true
							}
						}

						exportedFuncs = append(exportedFuncs, ExportedFunc{
							GoName:   funcDecl.Name.Name,
							JSName:   jsName,
							Params:   params,
							Results:  results,
							HasError: hasError,
						})
					}
				}

				if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.TYPE {
					hasGenExport, genCustomName := parseWagoExport(genDecl.Doc)
					for _, spec := range genDecl.Specs {
						typeSpec, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						structType, ok := typeSpec.Type.(*ast.StructType)
						if !ok {
							continue
						}
						structName := typeSpec.Name.Name
						hasSpecExport, specCustomName := parseWagoExport(typeSpec.Doc)

						stInfo := &StructInfo{
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
								stInfo.Fields = append(stInfo.Fields, fInfo)
							}
						}
						allStructs[structName] = stInfo

						if hasGenExport || hasSpecExport {
							annotatedStructs[structName] = true
							_ = genCustomName
							_ = specCustomName
						}
					}
				}
			}
		}
	}

	if pkgName == "" {
		pkgName = "main"
	}

	// Resolve target structs to generate
	resolvedStructs := make(map[string]*StructInfo)
	for name, st := range allStructs {
		if contains(cliTypes, name) || annotatedStructs[name] {
			resolvedStructs[name] = st
		}
	}

	// Transitive dependency resolver
	for _, fn := range exportedFuncs {
		for _, p := range fn.Params {
			resolveDependencies(p, allStructs, resolvedStructs)
		}
		for _, r := range fn.Results {
			resolveDependencies(r, allStructs, resolvedStructs)
		}
	}

	// Walk fields of resolved structs to find nested structs
	added := true
	for added {
		added = false
		for _, st := range resolvedStructs {
			for _, f := range st.Fields {
				if f.Kind == KindStruct || f.Kind == KindSlice || f.Kind == KindMap {
					if _, found := allStructs[f.BaseType]; found {
						if _, exists := resolvedStructs[f.BaseType]; !exists {
							resolvedStructs[f.BaseType] = allStructs[f.BaseType]
							added = true
						}
					}
				}
			}
		}
	}

	// If no structs and no functions are marked, exit
	if len(resolvedStructs) == 0 && len(exportedFuncs) == 0 {
		fmt.Fprintln(os.Stderr, "No structs or functions found to generate. Add //wago:export comments or use -type flag.")
		os.Exit(0)
	}

	// Sort struct names for deterministic output
	var structNames []string
	for name := range resolvedStructs {
		structNames = append(structNames, name)
	}
	// Sort structNames
	sortStrings(structNames)

	var goOut, jsOut string
	defaultBaseName := "wago_generated"
	if len(structNames) > 0 {
		defaultBaseName = strings.ToLower(structNames[0])
	} else if gofile != "" {
		ext := filepath.Ext(gofile)
		defaultBaseName = gofile[:len(gofile)-len(ext)]
	}

	if outputVal != "" {
		goOut = outputVal
	} else if gofile != "" {
		ext := filepath.Ext(gofile)
		base := gofile[:len(gofile)-len(ext)]
		goOut = base + "_wago.go"
	} else {
		goOut = defaultBaseName + "_wago.go"
	}

	if jsOutputVal != "" {
		jsOut = jsOutputVal
	} else if gofile != "" {
		ext := filepath.Ext(gofile)
		base := gofile[:len(gofile)-len(ext)]
		jsOut = base + ".js"
	} else {
		jsOut = defaultBaseName + ".js"
	}

	goCode, err := generateGoCode(pkgName, structNames, resolvedStructs, exportedFuncs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating Go code: %v\n", err)
		os.Exit(1)
	}

	jsCode := generateJSCode(structNames, resolvedStructs, exportedFuncs)

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

	if pkgName == "main" && !hasUserMain {
		mainWagoPath := filepath.Join(dir, "main_wago.go")
		var mainBuf bytes.Buffer
		mainBuf.WriteString("//go:build js && wasm\n\n")
		mainBuf.WriteString("// Code generated by wago; DO NOT EDIT.\n\n")
		mainBuf.WriteString("package main\n\n")
		if len(exportedFuncs) > 0 {
			mainBuf.WriteString("func main() {\n")
			mainBuf.WriteString("\tkeepAlive := make(chan struct{})\n")
			mainBuf.WriteString("\tRegisterWagoExports()\n")
			mainBuf.WriteString("\t<-keepAlive\n")
			mainBuf.WriteString("}\n")
		} else {
			mainBuf.WriteString("func main() {\n")
			mainBuf.WriteString("\tkeepAlive := make(chan struct{})\n")
			mainBuf.WriteString("\t<-keepAlive\n")
			mainBuf.WriteString("}\n")
		}
		formattedMain, err := format.Source(mainBuf.Bytes())
		if err == nil {
			err = os.WriteFile(mainWagoPath, formattedMain, 0644)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error writing main_wago.go: %v\n", err)
			} else {
				fmt.Printf("Generated main file: %s\n", mainWagoPath)
			}
		}
	}
}

func parseFuncParams(fset *token.FileSet, list []*ast.Field) []FieldInfo {
	var fields []FieldInfo
	idx := 0
	for _, field := range list {
		names := field.Names
		if len(names) == 0 {
			name := fmt.Sprintf("arg%d", idx)
			fInfo := parseField(fset, name, field.Type)
			fields = append(fields, fInfo)
			idx++
		} else {
			for _, nameIdent := range names {
				fInfo := parseField(fset, nameIdent.Name, field.Type)
				fields = append(fields, fInfo)
				idx++
			}
		}
	}
	return fields
}

func resolveDependencies(f FieldInfo, allStructs map[string]*StructInfo, resolved map[string]*StructInfo) {
	if f.Kind == KindStruct || f.Kind == KindSlice || f.Kind == KindMap {
		if _, found := allStructs[f.BaseType]; found {
			if _, exists := resolved[f.BaseType]; !exists {
				resolved[f.BaseType] = allStructs[f.BaseType]
				for _, subF := range allStructs[f.BaseType].Fields {
					resolveDependencies(subF, allStructs, resolved)
				}
			}
		}
	}
}

func contains(arr []string, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

func sortStrings(arr []string) {
	for i := 0; i < len(arr); i++ {
		for j := i + 1; j < len(arr); j++ {
			if arr[i] > arr[j] {
				arr[i], arr[j] = arr[j], arr[i]
			}
		}
	}
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

	allUpper := true
	for _, r := range runes {
		if unicode.IsLower(r) {
			allUpper = false
			break
		}
	}
	if allUpper {
		return strings.ToLower(s)
	}

	for i := 0; i < len(runes); i++ {
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

func generateGoCode(pkgName string, structNames []string, structs map[string]*StructInfo, exportedFuncs []ExportedFunc) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("//go:build js && wasm\n\n")
	buf.WriteString("// Code generated by wago; DO NOT EDIT.\n\n")
	buf.WriteString(fmt.Sprintf("package %s\n\n", pkgName))
	buf.WriteString("import \"syscall/js\"\n\n")

	// Struct mappings
	for _, tName := range structNames {
		st := structs[tName]
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
				buf.WriteString("\t\tfor i := 0; i < temp.Length(); i++ {\n")
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

	// RegisterWagoExports function
	if len(exportedFuncs) > 0 {
		buf.WriteString("func RegisterWagoExports() {\n")
		for _, fn := range exportedFuncs {
			buf.WriteString(fmt.Sprintf("\tjs.Global().Set(\"%s\", js.FuncOf(func(this js.Value, args []js.Value) any {\n", fn.JSName))

			// Write parameter deserialization
			for idx, p := range fn.Params {
				argName := fmt.Sprintf("arg_%s", p.GoName)
				switch p.Kind {
				case KindBasic:
					conv := generateGoConv(fmt.Sprintf("args[%d]", idx), p.BaseType)
					if p.IsPointer {
						buf.WriteString(fmt.Sprintf("\t\tif args[%d].IsNull() || args[%d].IsUndefined() {\n", idx, idx))
						buf.WriteString(fmt.Sprintf("\t\t\tvar val_%s %s\n", p.GoName, p.BaseType))
						buf.WriteString(fmt.Sprintf("\t\t\t_ = val_%s\n", p.GoName))
						buf.WriteString("\t\t} else {\n")
						buf.WriteString(fmt.Sprintf("\t\t\tval_%s := %s\n", p.GoName, conv))
						buf.WriteString(fmt.Sprintf("\t\t\t%s := &val_%s\n", argName, p.GoName))
						buf.WriteString(fmt.Sprintf("\t\t\t_ = %s\n", argName))
						buf.WriteString("\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t%s := %s\n", argName, conv))
						buf.WriteString(fmt.Sprintf("\t\t_ = %s\n", argName))
					}
				case KindStruct:
					if p.IsPointer {
						buf.WriteString(fmt.Sprintf("\t\tvar %s *%s\n", argName, p.BaseType))
						buf.WriteString(fmt.Sprintf("\t\tif !args[%d].IsNull() && !args[%d].IsUndefined() {\n", idx, idx))
						buf.WriteString(fmt.Sprintf("\t\t\tval_%s := %sFromJSValue(args[%d])\n", p.GoName, p.BaseType, idx))
						buf.WriteString(fmt.Sprintf("\t\t\t%s = &val_%s\n", argName, p.GoName))
						buf.WriteString("\t\t}\n")
					} else {
						buf.WriteString(fmt.Sprintf("\t\t%s := %sFromJSValue(args[%d])\n", argName, p.BaseType, idx))
						buf.WriteString(fmt.Sprintf("\t\t_ = %s\n", argName))
					}
				case KindSlice:
					buf.WriteString(fmt.Sprintf("\t\tvar %s %s\n", argName, p.GoType))
					buf.WriteString(fmt.Sprintf("\t\tif !args[%d].IsNull() && !args[%d].IsUndefined() {\n", idx, idx))
					buf.WriteString(fmt.Sprintf("\t\t\t%s = make(%s, args[%d].Length())\n", argName, p.GoType, idx))
					buf.WriteString(fmt.Sprintf("\t\t\tfor i := 0; i < args[%d].Length(); i++ {\n", idx))
					buf.WriteString(fmt.Sprintf("\t\t\t\titem := args[%d].Index(i)\n", idx))
					buf.WriteString("\t\t\t\tif !item.IsUndefined() && !item.IsNull() {\n")
					if p.EltIsBasic {
						conv := generateGoConv("item", p.BaseType)
						if p.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\tvalItem := %s\n", conv))
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[i] = &valItem\n", argName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[i] = %s\n", argName, conv))
						}
					} else {
						if p.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\tvalItem := %sFromJSValue(item)\n", p.BaseType))
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[i] = &valItem\n", argName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[i] = %sFromJSValue(item)\n", argName, p.BaseType))
						}
					}
					buf.WriteString("\t\t\t\t}\n")
					buf.WriteString("\t\t\t}\n")
					buf.WriteString("\t\t}\n")
				case KindMap:
					buf.WriteString(fmt.Sprintf("\t\tvar %s %s\n", argName, p.GoType))
					buf.WriteString(fmt.Sprintf("\t\tif !args[%d].IsNull() && !args[%d].IsUndefined() {\n", idx, idx))
					buf.WriteString(fmt.Sprintf("\t\t\t%s = make(%s)\n", argName, p.GoType))
					buf.WriteString(fmt.Sprintf("\t\t\tkeys := js.Global().Get(\"Object\").Call(\"keys\", args[%d])\n", idx))
					buf.WriteString("\t\t\tfor i := 0; i < keys.Length(); i++ {\n")
					buf.WriteString("\t\t\t\tk := keys.Index(i).String()\n")
					buf.WriteString(fmt.Sprintf("\t\t\t\titem := args[%d].Get(k)\n", idx))
					buf.WriteString("\t\t\t\tif !item.IsUndefined() && !item.IsNull() {\n")
					if p.EltIsBasic {
						conv := generateGoConv("item", p.BaseType)
						if p.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\tvalItem := %s\n", conv))
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[k] = &valItem\n", argName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[k] = %s\n", argName, conv))
						}
					} else {
						if p.EltIsPtr {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\tvalItem := %sFromJSValue(item)\n", p.BaseType))
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[k] = &valItem\n", argName))
						} else {
							buf.WriteString(fmt.Sprintf("\t\t\t\t\t%s[k] = %sFromJSValue(item)\n", argName, p.BaseType))
						}
					}
					buf.WriteString("\t\t\t\t}\n")
					buf.WriteString("\t\t\t}\n")
					buf.WriteString("\t\t}\n")
				}
			}

			// Call Go function
			var argCallNames []string
			for _, p := range fn.Params {
				argCallNames = append(argCallNames, fmt.Sprintf("arg_%s", p.GoName))
			}
			callExpr := fmt.Sprintf("%s(%s)", fn.GoName, strings.Join(argCallNames, ", "))

			if len(fn.Results) == 0 {
				buf.WriteString(fmt.Sprintf("\t\t%s\n", callExpr))
				buf.WriteString("\t\treturn nil\n")
			} else if len(fn.Results) == 1 {
				if fn.HasError {
					buf.WriteString(fmt.Sprintf("\t\terr := %s\n", callExpr))
					buf.WriteString("\t\tif err != nil {\n")
					buf.WriteString("\t\t\treturn js.Global().Get(\"Error\").New(err.Error())\n")
					buf.WriteString("\t\t}\n")
					buf.WriteString("\t\treturn nil\n")
				} else {
					buf.WriteString(fmt.Sprintf("\t\tres := %s\n", callExpr))
					buf.WriteString(generateGoToJSReturnValue("res", fn.Results[0]))
				}
			} else if len(fn.Results) == 2 && fn.HasError {
				buf.WriteString(fmt.Sprintf("\t\tres, err := %s\n", callExpr))
				buf.WriteString("\t\tif err != nil {\n")
				buf.WriteString("\t\t\treturn js.Global().Get(\"Error\").New(err.Error())\n")
				buf.WriteString("\t\t}\n")
				buf.WriteString(generateGoToJSReturnValue("res", fn.Results[0]))
			} else {
				// Unsupported multiple results
				buf.WriteString("\t\t// Warning: multiple return values are not supported except error\n")
				buf.WriteString(fmt.Sprintf("\t\t%s\n", callExpr))
				buf.WriteString("\t\treturn nil\n")
			}

			buf.WriteString("\t}))\n")
		}
		buf.WriteString("}\n\n")
	}

	return format.Source(buf.Bytes())
}

func generateGoToJSReturnValue(varName string, r FieldInfo) string {
	var buf bytes.Buffer
	switch r.Kind {
	case KindBasic:
		if r.IsPointer {
			buf.WriteString(fmt.Sprintf("\t\tif %s != nil {\n", varName))
			buf.WriteString(fmt.Sprintf("\t\t\treturn js.ValueOf(*%s)\n", varName))
			buf.WriteString("\t\t}\n")
			buf.WriteString("\t\treturn js.Null()\n")
		} else {
			buf.WriteString(fmt.Sprintf("\t\treturn js.ValueOf(%s)\n", varName))
		}
	case KindStruct:
		if r.IsPointer {
			buf.WriteString(fmt.Sprintf("\t\tif %s != nil {\n", varName))
			buf.WriteString(fmt.Sprintf("\t\t\treturn %s.ToJSValue()\n", varName))
			buf.WriteString("\t\t}\n")
			buf.WriteString("\t\treturn js.Null()\n")
		} else {
			buf.WriteString(fmt.Sprintf("\t\treturn %s.ToJSValue()\n", varName))
		}
	case KindSlice:
		buf.WriteString(fmt.Sprintf("\t\tif %s != nil {\n", varName))
		buf.WriteString(fmt.Sprintf("\t\t\tarrRes := js.Global().Get(\"Array\").New(len(%s))\n", varName))
		buf.WriteString(fmt.Sprintf("\t\t\tfor idx, val := range %s {\n", varName))
		if r.EltIsBasic {
			if r.EltIsPtr {
				buf.WriteString("\t\t\t\tif val != nil {\n")
				buf.WriteString("\t\t\t\t\tarrRes.SetIndex(idx, js.ValueOf(*val))\n")
				buf.WriteString("\t\t\t\t} else {\n")
				buf.WriteString("\t\t\t\t\tarrRes.SetIndex(idx, js.Null())\n")
				buf.WriteString("\t\t\t\t}\n")
			} else {
				buf.WriteString("\t\t\t\tarrRes.SetIndex(idx, js.ValueOf(val))\n")
			}
		} else {
			if r.EltIsPtr {
				buf.WriteString("\t\t\t\tif val != nil {\n")
				buf.WriteString("\t\t\t\t\tarrRes.SetIndex(idx, val.ToJSValue())\n")
				buf.WriteString("\t\t\t\t} else {\n")
				buf.WriteString("\t\t\t\t\tarrRes.SetIndex(idx, js.Null())\n")
				buf.WriteString("\t\t\t\t}\n")
			} else {
				buf.WriteString("\t\t\t\tarrRes.SetIndex(idx, val.ToJSValue())\n")
			}
		}
		buf.WriteString("\t\t\t}\n")
		buf.WriteString("\t\t\treturn arrRes\n")
		buf.WriteString("\t\t}\n")
		buf.WriteString("\t\treturn js.Null()\n")
	case KindMap:
		buf.WriteString(fmt.Sprintf("\t\tif %s != nil {\n", varName))
		buf.WriteString("\t\t\tmapRes := js.Global().Get(\"Object\").New()\n")
		buf.WriteString(fmt.Sprintf("\t\t\tfor k, val := range %s {\n", varName))
		if r.EltIsBasic {
			if r.EltIsPtr {
				buf.WriteString("\t\t\t\tif val != nil {\n")
				buf.WriteString("\t\t\t\t\tmapRes.Set(k, js.ValueOf(*val))\n")
				buf.WriteString("\t\t\t\t} else {\n")
				buf.WriteString("\t\t\t\t\tmapRes.Set(k, js.Null())\n")
				buf.WriteString("\t\t\t\t}\n")
			} else {
				buf.WriteString("\t\t\t\tmapRes.Set(k, js.ValueOf(val))\n")
			}
		} else {
			if r.EltIsPtr {
				buf.WriteString("\t\t\t\tif val != nil {\n")
				buf.WriteString("\t\t\t\t\tmapRes.Set(k, val.ToJSValue())\n")
				buf.WriteString("\t\t\t\t} else {\n")
				buf.WriteString("\t\t\t\t\tmapRes.Set(k, js.Null())\n")
				buf.WriteString("\t\t\t\t}\n")
			} else {
				buf.WriteString("\t\t\t\tmapRes.Set(k, val.ToJSValue())\n")
			}
		}
		buf.WriteString("\t\t\t}\n")
		buf.WriteString("\t\t\treturn mapRes\n")
		buf.WriteString("\t\t}\n")
		buf.WriteString("\t\treturn js.Null()\n")
	}
	return buf.String()
}

func generateJSCode(structNames []string, structs map[string]*StructInfo, exportedFuncs []ExportedFunc) string {
	var buf bytes.Buffer

	buf.WriteString("// Code generated by wago; DO NOT EDIT.\n\n")

	// Class declarations
	for _, tName := range structNames {
		st := structs[tName]
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

	// Function wrappers
	for _, fn := range exportedFuncs {
		// JSDoc
		buf.WriteString("/**\n")
		for _, p := range fn.Params {
			jsDocType := getJSDocType(p)
			buf.WriteString(fmt.Sprintf(" * @param {%s} %s\n", jsDocType, p.GoName))
		}
		if len(fn.Results) > 0 {
			retType := getJSDocType(fn.Results[0])
			if fn.HasError {
				if len(fn.Results) == 1 {
					retType = "void"
				}
			}
			buf.WriteString(fmt.Sprintf(" * @returns {%s}\n", retType))
		} else {
			buf.WriteString(" * @returns {void}\n")
		}
		buf.WriteString(" */\n")

		// Function definition
		var paramNames []string
		for _, p := range fn.Params {
			paramNames = append(paramNames, p.GoName)
		}
		buf.WriteString(fmt.Sprintf("export function %s(%s) {\n", fn.JSName, strings.Join(paramNames, ", ")))

		// Convert arguments
		var argCallNames []string
		for _, p := range fn.Params {
			rawName := fmt.Sprintf("raw_%s", p.GoName)
			switch p.Kind {
			case KindBasic:
				buf.WriteString(fmt.Sprintf("\tconst %s = %s;\n", rawName, p.GoName))
			case KindStruct:
				if p.IsPointer {
					buf.WriteString(fmt.Sprintf("\tconst %s = %s ? %s.toJS() : null;\n", rawName, p.GoName, p.GoName))
				} else {
					buf.WriteString(fmt.Sprintf("\tconst %s = %s ? %s.toJS() : null;\n", rawName, p.GoName, p.GoName))
				}
			case KindSlice:
				if p.EltIsBasic {
					buf.WriteString(fmt.Sprintf("\tconst %s = %s || [];\n", rawName, p.GoName))
				} else {
					buf.WriteString(fmt.Sprintf("\tconst %s = %s ? %s.map(item => item ? item.toJS() : null) : [];\n", rawName, p.GoName, p.GoName))
				}
			case KindMap:
				if p.EltIsBasic {
					buf.WriteString(fmt.Sprintf("\tconst %s = %s || {};\n", rawName, p.GoName))
				} else {
					buf.WriteString(fmt.Sprintf("\tconst %s = (() => { const res = {}; if (%s) { for (const k in %s) { res[k] = %s[k] ? %s[k].toJS() : null; } } return res; })();\n", rawName, p.GoName, p.GoName, p.GoName, p.GoName))
				}
			}
			argCallNames = append(argCallNames, rawName)
		}

		// Call global function
		callExpr := fmt.Sprintf("globalThis.%s(%s)", fn.JSName, strings.Join(argCallNames, ", "))
		buf.WriteString(fmt.Sprintf("\tconst res = %s;\n", callExpr))

		// Check error
		if fn.HasError {
			buf.WriteString("\tif (res instanceof Error) {\n\t\tthrow res;\n\t}\n")
		}

		// Return conversion
		if len(fn.Results) == 0 || (fn.HasError && len(fn.Results) == 1) {
			buf.WriteString("\treturn;\n")
		} else {
			retField := fn.Results[0]
			switch retField.Kind {
			case KindBasic:
				buf.WriteString("\treturn res;\n")
			case KindStruct:
				buf.WriteString(fmt.Sprintf("\treturn %s.fromJS(res);\n", retField.BaseType))
			case KindSlice:
				if retField.EltIsBasic {
					buf.WriteString("\treturn res;\n")
				} else {
					buf.WriteString(fmt.Sprintf("\treturn res ? res.map(item => %s.fromJS(item)) : [];\n", retField.BaseType))
				}
			case KindMap:
				if retField.EltIsBasic {
					buf.WriteString("\treturn res;\n")
				} else {
					buf.WriteString(fmt.Sprintf("\treturn (() => { const out = {}; if (res) { for (const k in res) { out[k] = %s.fromJS(res[k]); } } return out; })();\n", retField.BaseType))
				}
			}
		}

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
