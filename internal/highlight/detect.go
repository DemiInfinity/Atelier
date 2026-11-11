package highlight

import (
	"path"
	"sort"
	"strings"

	"github.com/alecthomas/chroma/v2/lexers"
)

// Lang is a detected language.
type Lang struct {
	Name  string // shown to people; matches the language bar's names where both know it
	Lexer string // chroma lexer name; "" means plain text
}

var (
	byExt  = map[string]Lang{} // ".kt"
	byName = map[string]Lang{} // "Dockerfile"
	byID   = map[string]Lang{} // lower-case display name, for the editor's picker
)

func init() {
	add := func(name, lexer string, keys ...string) {
		l := Lang{Name: name, Lexer: lexer}
		if lexer != "" && lexers.Get(lexer) == nil {
			panic("highlight: no chroma lexer " + lexer)
		}
		if _, ok := byID[strings.ToLower(name)]; !ok {
			byID[strings.ToLower(name)] = l
		}
		for _, k := range keys {
			if strings.HasPrefix(k, ".") {
				byExt[k] = l
			} else {
				byName[k] = l
			}
		}
	}
	// Names follow internal/web/insights_git.go (langByExt) so a file's
	// language reads the same in the language bar and on its page.
	add("Go", "Go", ".go")
	add("Go Module", "Go", "go.mod", "go.work")
	add("TypeScript", "TypeScript", ".ts", ".tsx", ".mts", ".cts")
	add("JavaScript", "JavaScript", ".js", ".mjs", ".cjs")
	add("JavaScript", "react", ".jsx")
	add("Svelte", "Svelte", ".svelte")
	add("Vue", "vue", ".vue")
	add("Astro", "HTML", ".astro")
	add("Kotlin", "Kotlin", ".kt", ".kts")
	add("Java", "Java", ".java")
	add("Swift", "Swift", ".swift")
	add("Objective-C", "Objective-C", ".m", ".mm")
	add("C", "C", ".c", ".h")
	add("C++", "C++", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ino")
	add("C#", "C#", ".cs", ".csx")
	add("Rust", "Rust", ".rs")
	add("Python", "Python", ".py", ".pyi", ".pyw", "SConstruct", "SConscript")
	add("Ruby", "Ruby", ".rb", ".rake", ".gemspec", "Gemfile", "Rakefile", "Podfile", "Fastfile", "Brewfile")
	add("PHP", "PHP", ".php")
	add("Dart", "Dart", ".dart")
	add("Shell", "Bash", ".sh", ".bash", ".zsh", ".ksh", ".bashrc", ".zshrc", ".profile", ".bash_profile", "PKGBUILD")
	add("Shell", "Fish", ".fish")
	add("PowerShell", "PowerShell", ".ps1", ".psm1", ".psd1")
	add("Batchfile", "Batchfile", ".bat", ".cmd")
	add("SQL", "SQL", ".sql")
	add("YAML", "YAML", ".yaml", ".yml", ".clang-format", ".clangd")
	add("TOML", "TOML", ".toml", "Cargo.lock", "Pipfile", "poetry.lock", "uv.lock")
	add("JSON", "JSON", ".json", ".jsonc", ".json5", ".jsonl", ".webmanifest", ".babelrc", ".eslintrc", ".prettierrc", "composer.lock", ".tsbuildinfo")
	add("XML", "XML", ".xml", ".svg", ".plist", ".xsd", ".xsl", ".xslt", ".csproj", ".fsproj", ".vbproj", ".storyboard", ".xib", ".resx", ".iml")
	add("HTML", "HTML", ".html", ".htm", ".xhtml")
	add("HTML", "Go HTML Template", ".tmpl", ".gohtml")
	add("CSS", "CSS", ".css")
	add("SCSS", "SCSS", ".scss")
	add("SCSS", "Sass", ".sass")
	add("Less", "SCSS", ".less")
	add("Markdown", "markdown", ".md", ".markdown", ".mkd", ".mdx")
	add("Dockerfile", "Docker", "Dockerfile", "Containerfile", ".dockerfile")
	add("Makefile", "Makefile", "Makefile", "GNUmakefile", "makefile", "BSDmakefile", ".mk", ".mak")
	add("CMake", "CMake", "CMakeLists.txt", ".cmake")
	add("Groovy", "Groovy", ".groovy", ".gradle", "Jenkinsfile")
	add("Protocol Buffer", "Protocol Buffer", ".proto")
	add("GraphQL", "GraphQL", ".graphql", ".graphqls", ".gql")
	add("Lua", "Lua", ".lua")
	add("Elixir", "Elixir", ".ex", ".exs", ".eex", ".heex")
	add("Erlang", "Erlang", ".erl", ".hrl")
	add("Haskell", "Haskell", ".hs", ".lhs")
	add("Zig", "Zig", ".zig", ".zon")
	add("Nix", "Nix", ".nix")
	add("HCL", "Terraform", ".tf", ".tfvars", ".hcl")
	add("INI", "INI", ".ini", ".cfg", ".conf", ".editorconfig", ".gitconfig", ".npmrc", ".service", ".desktop", "setup.cfg", "tox.ini")
	add("Env", "Bash", ".env", ".envrc")
	add("Properties", "properties", ".properties")
	add("Perl", "Perl", ".pl", ".pm")
	add("R", "R", ".r", ".R")
	add("Scala", "Scala", ".scala", ".sbt")
	add("Clojure", "Clojure", ".clj", ".cljs", ".cljc", ".edn")
	add("OCaml", "OCaml", ".ml", ".mli")
	add("Elm", "Elm", ".elm")
	add("Julia", "Julia", ".jl")
	add("Solidity", "Solidity", ".sol")
	add("GLSL", "GLSL", ".glsl", ".frag", ".vert")
	add("Metal", "C++", ".metal")
	add("WebAssembly", "", ".wat")
	add("Vim Script", "VimL", ".vim", ".vimrc")
	add("Diff", "Diff", ".diff", ".patch")
	add("Nginx", "nginx configuration file", "nginx.conf")
	add("Caddyfile", "Caddyfile", "Caddyfile")
	add("Git config", "INI", ".gitmodules")
	add("Ignore file", "", ".gitignore", ".dockerignore", ".npmignore")
	add("Plain text", "", ".txt", ".text", ".log")
}

// shebang interpreters
var byInterp = map[string]string{
	"sh": ".sh", "bash": ".sh", "zsh": ".sh", "ksh": ".sh", "dash": ".sh", "fish": ".fish",
	"python": ".py", "python3": ".py", "python2": ".py",
	"node": ".js", "deno": ".ts", "bun": ".ts", "tsx": ".ts", "ts-node": ".ts",
	"ruby": ".rb", "perl": ".pl", "php": ".php", "lua": ".lua", "elixir": ".exs",
	"pwsh": ".ps1", "make": ".mk", "kotlin": ".kts", "swift": ".swift", "Rscript": ".r",
}

// Detect picks the language of a file: by its name or extension first, then
// by its #! line, then (small files only) by chroma's content analysers.
func Detect(name string, data []byte) Lang {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	if l, ok := byName[base]; ok {
		return l
	}
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, ".gradle.kts"):
		return byExt[".kts"]
	case strings.HasPrefix(base, "Dockerfile.") || strings.HasPrefix(base, "Containerfile.") || strings.HasSuffix(lower, ".dockerfile"):
		return byName["Dockerfile"]
	case strings.HasPrefix(lower, ".env.") || lower == ".env":
		return byExt[".env"]
	case strings.HasPrefix(base, "Makefile."):
		return byName["Makefile"]
	case lower == "dockerfile" || lower == "containerfile":
		return byName["Dockerfile"]
	}
	ext := path.Ext(base)
	if ext == "" && strings.HasPrefix(base, ".") {
		ext = base // dotfiles: ".bashrc"
	}
	if l, ok := byExt[ext]; ok {
		return l
	}
	if l, ok := byExt[strings.ToLower(ext)]; ok {
		return l
	}
	if l, ok := shebang(data); ok {
		return l
	}
	// chroma's own filename patterns cover many more languages.
	if lx := lexers.Match(base); lx != nil {
		return Lang{Name: lx.Config().Name, Lexer: lx.Config().Name}
	}
	if ext == "" && len(data) > 0 && len(data) <= analyseMax {
		if lx := lexers.Analyse(string(data)); lx != nil {
			n := lx.Config().Name
			if n != "plaintext" && n != "Plaintext" {
				return Lang{Name: n, Lexer: n}
			}
		}
	}
	return Lang{}
}

func shebang(data []byte) (Lang, bool) {
	if len(data) < 3 || data[0] != '#' || data[1] != '!' {
		return Lang{}, false
	}
	line := string(data[2:])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return Lang{}, false
	}
	interp := path.Base(f[0])
	if interp == "env" {
		interp = ""
		for _, a := range f[1:] {
			if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
				interp = path.Base(a)
				break
			}
		}
	}
	// python3.12 → python3
	for k := interp; k != ""; k = k[:len(k)-1] {
		if ext, ok := byInterp[k]; ok {
			return byExt[ext], true
		}
		if !strings.ContainsAny(k[len(k)-1:], ".0123456789") {
			break
		}
	}
	return Lang{}, false
}

// LexerForHint maps a Markdown fence's info word or the editor's language
// choice to a chroma lexer name: a language name from the picker ("kotlin"),
// a chroma name or alias ("golang", "ts", "sh") or an extension ("kt").
func LexerForHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	h := strings.ToLower(hint)
	if l, ok := byID[h]; ok {
		return l.Lexer
	}
	switch h {
	case "text", "plain", "plaintext", "txt", "none", "output", "console-output":
		return ""
	case "jsx":
		return "react"
	case "tsx", "ts":
		return "TypeScript"
	case "kts":
		return "Kotlin"
	case "env", "dotenv":
		return "Bash"
	case "tf", "terraform", "hcl":
		return "Terraform"
	case "objc", "objective-c", "objectivec":
		return "Objective-C"
	}
	if l, ok := byExt["."+h]; ok {
		return l.Lexer
	}
	if lx := lexers.Get(h); lx != nil {
		return lx.Config().Name
	}
	return ""
}

// Languages lists the language choices for the editor's picker.
func Languages() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range byID {
		if l.Lexer == "" || seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		out = append(out, l.Name)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}
