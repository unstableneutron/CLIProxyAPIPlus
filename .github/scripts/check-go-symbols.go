//go:build ignore

// Command check-go-symbols emits package-qualified declarations from shared
// upstream-sync hotspot paths at a Git ref or in the current worktree.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
)

type ownershipRule struct {
	class string
	path  string
}

func main() {
	ownershipPath := flag.String("ownership", ".github/upstream-sync-ownership.tsv", "ownership manifest")
	ref := flag.String("ref", "", "Git ref to scan; empty scans the worktree")
	flag.Parse()

	rules, err := readOwnership(*ownershipPath)
	if err != nil {
		fatal(err)
	}
	paths, err := gitLines(gitListArgs(*ref)...)
	if err != nil {
		fatal(err)
	}

	symbols := make(map[string]struct{})
	for _, filePath := range paths {
		if !strings.HasSuffix(filePath, ".go") || owned(filePath, rules) {
			continue
		}
		source, errRead := readSource(*ref, filePath)
		if errRead != nil {
			fatal(errRead)
		}
		if source == nil {
			continue
		}
		file, errParse := parser.ParseFile(token.NewFileSet(), filePath, source, parser.SkipObjectResolution)
		if errParse != nil {
			fatal(fmt.Errorf("parse %s: %w", filePath, errParse))
		}
		collectFileSymbols(path.Dir(filePath), file, symbols)
	}

	ordered := make([]string, 0, len(symbols))
	for symbol := range symbols {
		ordered = append(ordered, symbol)
	}
	sort.Strings(ordered)
	for _, symbol := range ordered {
		fmt.Println(symbol)
	}
}

func gitListArgs(ref string) []string {
	if ref == "" {
		return []string{"ls-files", "--", "internal", "sdk", "cmd"}
	}
	return []string{"ls-tree", "-r", "--name-only", ref, "--", "internal", "sdk", "cmd"}
}

func gitLines(args ...string) ([]string, error) {
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

func readSource(ref, filePath string) ([]byte, error) {
	if ref == "" {
		source, err := os.ReadFile(filePath)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
		return source, nil
	}
	output, err := exec.Command("git", "show", ref+":"+filePath).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", ref, filePath, err)
	}
	return output, nil
}

func readOwnership(filePath string) ([]ownershipRule, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open ownership manifest: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "check-go-symbols: close ownership manifest: %v\n", closeErr)
		}
	}()

	var rules []ownershipRule
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || parts[1] == "" {
			return nil, fmt.Errorf("invalid ownership rule: %q", line)
		}
		rules = append(rules, ownershipRule{class: parts[0], path: parts[1]})
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read ownership manifest: %w", errScan)
	}
	return rules, nil
}

func owned(filePath string, rules []ownershipRule) bool {
	for _, rule := range rules {
		if rule.class != "plus-owned" && rule.class != "fork-owned" {
			continue
		}
		if strings.HasSuffix(rule.path, "/") {
			if strings.HasPrefix(filePath, rule.path) {
				return true
			}
		} else if filePath == rule.path {
			return true
		}
	}
	return false
}

func collectFileSymbols(packagePath string, file *ast.File, symbols map[string]struct{}) {
	for _, declaration := range file.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			name := node.Name.Name
			kind := "func"
			if node.Recv != nil && len(node.Recv.List) > 0 {
				receiver := receiverName(node.Recv.List[0].Type)
				if receiver == "" {
					continue
				}
				kind = "method"
				name = receiver + "." + name
			}
			addSymbol(symbols, packagePath, kind, name)
		case *ast.GenDecl:
			for _, specification := range node.Specs {
				switch spec := specification.(type) {
				case *ast.TypeSpec:
					addSymbol(symbols, packagePath, "type", spec.Name.Name)
					if iface, ok := spec.Type.(*ast.InterfaceType); ok {
						for _, field := range iface.Methods.List {
							for _, name := range field.Names {
								addSymbol(symbols, packagePath, "interface-method", spec.Name.Name+"."+name.Name)
							}
						}
					}
				case *ast.ValueSpec:
					kind := strings.ToLower(node.Tok.String())
					for _, name := range spec.Names {
						addSymbol(symbols, packagePath, kind, name.Name)
					}
				}
			}
		}
	}
}

func receiverName(expression ast.Expr) string {
	switch node := expression.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.StarExpr:
		return receiverName(node.X)
	case *ast.IndexExpr:
		return receiverName(node.X)
	case *ast.IndexListExpr:
		return receiverName(node.X)
	default:
		return ""
	}
}

func addSymbol(symbols map[string]struct{}, packagePath, kind, name string) {
	symbols[packagePath+"|"+kind+"|"+name] = struct{}{}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "check-go-symbols: %v\n", err)
	os.Exit(1)
}
