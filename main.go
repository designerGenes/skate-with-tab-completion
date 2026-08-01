// Package main provides the skate CLI.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agnivade/levenshtein"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/lipgloss"
	"github.com/dgraph-io/badger/v4"
	gap "github.com/muesli/go-app-paths"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	reverseIterate   bool
	keysIterate      bool
	valuesIterate    bool
	showBinary       bool
	delimiterIterate string

	warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Bold(true)

	rootCmd = &cobra.Command{
		Use:   "skate",
		Short: "Skate, a personal key value store.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	setCmd = &cobra.Command{
		Use:     "set KEY[@DB] [VALUE] or set DB KEY [VALUE]",
		Short:   "Set a value for a key with an optional @ db or DB name. If VALUE is omitted, read value from standard input.",
		Example: "  skate set foo bar\n  skate set key1@group1 value1\n  skate set group1 key1 value1\n  skate set foo <./bar.txt",
		Args:    cobra.RangeArgs(1, 3),
		RunE:    set,
	}

	getCmd = &cobra.Command{
		Use:           "get KEY[@DB] or get DB [KEY]",
		Short:         "Get a value for a key with an optional @ db or DB name.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.RangeArgs(1, 2),
		RunE:          get,
	}

	deleteCmd = &cobra.Command{
		Use:     "delete KEY[@DB] or delete DB KEY",
		Short:   "Delete a key with an optional @ db or DB name.",
		Aliases: []string{"del", "rm"},
		Args:    cobra.RangeArgs(1, 2),
		RunE:    del,
	}

	listCmd = &cobra.Command{
		Use:     "list [@DB]",
		Short:   "List key value pairs with an optional @ db.",
		Aliases: []string{"ls"},
		Args:    cobra.MaximumNArgs(1),
		RunE:    list,
	}

	listDbsCmd = &cobra.Command{
		Use:     "list-dbs",
		Short:   "List databases.",
		Aliases: []string{"ls-db"},
		Args:    cobra.NoArgs,
		RunE:    listDbs,
	}

	deleteDbCmd = &cobra.Command{
		Use:     "delete-db [@DB]",
		Hidden:  false,
		Short:   "Delete a database",
		Aliases: []string{"del-db", "rm-db"},
		Args:    cobra.MinimumNArgs(1),
		RunE:    deleteDb,
	}

	completionCmd = &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion script",
		Long: `To load completions:

Zsh:
  $ source <(skate completion zsh)

Bash:
  $ source <(skate completion bash)

Fish:
  $ skate completion fish | source
`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletion(os.Stdout)
			case "zsh":
				buf := new(bytes.Buffer)
				if err := cmd.Root().GenZshCompletion(buf); err != nil {
					return err
				}
				script := buf.String()
				// Prevent (eval):1: command not found: _skate error when words[1] is _skate or starts with _
				script = strings.Replace(script, `requestComp="${words[1]}`, `local compCmd="${words[1]}"
    if [[ "${compCmd}" == _* || -z "${compCmd}" ]]; then
        compCmd="skate"
    fi
    requestComp="${compCmd}`, 1)
				if strings.Contains(script, "compdef _skate skate") {
					script = strings.Replace(script, "compdef _skate skate", "if type compdef &>/dev/null; then\n    compdef _skate skate\nfi", 1)
				}
				_, err := fmt.Fprint(os.Stdout, script)
				return err
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
)

type errDBNotFound struct {
	suggestions []string
}

func (err errDBNotFound) Error() string {
	if len(err.suggestions) == 0 {
		return "no suggestions found"
	}
	return fmt.Sprintf("did you mean %q", strings.Join(err.suggestions, ", "))
}

//nolint:wrapcheck
func set(cmd *cobra.Command, args []string) error {
	var key []byte
	var dbName string
	var valStr string
	var hasVal bool

	switch len(args) {
	case 3:
		dbName = strings.ToLower(strings.TrimPrefix(args[0], "@"))
		key = []byte(strings.ToLower(args[1]))
		valStr = args[2]
		hasVal = true
	case 2:
		if strings.Contains(args[0], "@") {
			k, n, err := keyParser(args[0])
			if err != nil {
				return err
			}
			key = k
			dbName = n
			valStr = args[1]
			hasVal = true
		} else {
			dbs, _ := getRawDbs()
			isDb := false
			for _, d := range dbs {
				if strings.EqualFold(d, args[0]) {
					isDb = true
					break
				}
			}
			if isDb {
				dbName = strings.ToLower(args[0])
				key = []byte(strings.ToLower(args[1]))
				hasVal = false
			} else {
				key = []byte(strings.ToLower(args[0]))
				dbName = ""
				valStr = args[1]
				hasVal = true
			}
		}
	case 1:
		k, n, err := keyParser(args[0])
		if err != nil {
			return err
		}
		key = k
		dbName = n
		hasVal = false
	}

	db, err := openKV(dbName)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck

	if hasVal {
		return wrap(db, false, func(tx *badger.Txn) error {
			return tx.Set(key, []byte(valStr))
		})
	}

	bts, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return err
	}
	return wrap(db, false, func(tx *badger.Txn) error {
		return tx.Set(key, bts)
	})
}

//nolint:wrapcheck
func get(_ *cobra.Command, args []string) error {
	var key []byte
	var dbName string

	if len(args) == 2 {
		dbName = strings.ToLower(strings.TrimPrefix(args[0], "@"))
		key = []byte(strings.ToLower(args[1]))
	} else if len(args) == 1 {
		if strings.Contains(args[0], "@") {
			k, n, err := keyParser(args[0])
			if err != nil {
				return err
			}
			key = k
			dbName = n
		} else {
			dbs, _ := getRawDbs()
			isDb := false
			for _, d := range dbs {
				if strings.EqualFold(d, args[0]) {
					isDb = true
					break
				}
			}

			if isDb {
				dbDefault, err := openKVReadOnly("")
				keyExistsInDefault := false
				if err == nil {
					_ = dbDefault.View(func(tx *badger.Txn) error {
						_, err := tx.Get([]byte(strings.ToLower(args[0])))
						if err == nil {
							keyExistsInDefault = true
						}
						return nil
					})
					dbDefault.Close() //nolint:errcheck
				}

				if keyExistsInDefault {
					key = []byte(strings.ToLower(args[0]))
					dbName = ""
				} else {
					return listDbKeysAndValues(args[0])
				}
			} else {
				k, n, err := keyParser(args[0])
				if err != nil {
					return err
				}
				key = k
				dbName = n
			}
		}
	}

	db, err := openKV(dbName)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	var v []byte
	if err := wrap(db, true, func(tx *badger.Txn) error {
		item, err := tx.Get(key)
		if err != nil {
			return err
		}
		v, err = item.ValueCopy(nil)
		return err
	}); err != nil {
		return err
	}
	printFromKV("%s", v)
	return nil
}

func listDbKeysAndValues(dbName string) error {
	db, err := openKV(dbName)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()
			err := item.Value(func(v []byte) error {
				printFromKV("%s\t%s\n", k, v)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func del(_ *cobra.Command, args []string) error {
	var key []byte
	var dbName string

	if len(args) == 2 {
		dbName = strings.ToLower(strings.TrimPrefix(args[0], "@"))
		key = []byte(strings.ToLower(args[1]))
	} else if len(args) == 1 {
		k, n, err := keyParser(args[0])
		if err != nil {
			return err
		}
		key = k
		dbName = n
	}

	db, err := openKV(dbName)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck

	return wrap(db, false, func(tx *badger.Txn) error {
		return tx.Delete(key)
	})
}

// TODO: use lists/tables/trees for this?
func listDbs(*cobra.Command, []string) error {
	dbs, err := getDbs()
	for _, db := range dbs {
		fmt.Println(db)
	}
	return err
}

func getRawDbs() ([]string, error) {
	filepath, err := getFilePath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath)
	if err != nil {
		return nil, err
	}
	var dbList []string
	for _, e := range entries {
		if e.IsDir() {
			dbList = append(dbList, e.Name())
		}
	}
	return dbList, nil
}

// getDbs: returns a formatted list of available Skate DBs.
//
//nolint:wrapcheck
func getDbs() ([]string, error) {
	dbList, err := getRawDbs()
	if err != nil {
		return nil, err
	}
	return formatDbs(dbList), nil
}

func getKeysInDb(dbName string) ([]string, error) {
	db, err := openKVReadOnly(dbName)
	if err != nil {
		return nil, err
	}
	defer db.Close() //nolint:errcheck

	var keys []string
	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			keys = append(keys, string(item.Key()))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func openKVReadOnly(name string) (*badger.DB, error) {
	if name == "" {
		name = "default"
	}
	path, err := getFilePath(name)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); os.IsNotExist(err) || !info.IsDir() {
		return nil, fmt.Errorf("db does not exist")
	}
	opts := badger.DefaultOptions(path).WithLoggingLevel(badger.ERROR).WithReadOnly(true)
	return badger.Open(opts)
}

func formatDbs(dbs []string) []string {
	out := make([]string, 0, len(dbs))
	for _, db := range dbs {
		out = append(out, "@"+db)
	}
	return out
}

// getFilePath: get the file path to the skate databases.
//
//nolint:wrapcheck
func getFilePath(args ...string) (string, error) {
	scope := gap.NewScope(gap.User, "charm")
	dd, pathErr := scope.DataPath("")
	if pathErr != nil {
		return "", pathErr
	}
	dir := filepath.Join(dd, "kv")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, args...)...), nil
}

// deleteDb: delete a Skate database.
//
//nolint:wrapcheck
func deleteDb(_ *cobra.Command, args []string) error {
	path, err := findDb(args[0])
	var errNotFound errDBNotFound
	if errors.As(err, &errNotFound) {
		fmt.Fprintf(os.Stderr, "%q does not exist, %s\n", args[0], err.Error())
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "unexpected error: %s", err.Error())
		os.Exit(1)
	}
	var confirmation string

	home, err := os.UserHomeDir()
	showpath := path
	if err == nil && strings.HasPrefix(path, home) {
		showpath = filepath.Join("~", strings.TrimPrefix(showpath, home))
	}
	message := fmt.Sprintf("Are you sure you want to delete '%s' and all its contents? (y/n)", warningStyle.Render(showpath))
	message = lipgloss.NewStyle().Width(78).Render(message)
	fmt.Println(message)

	if _, err := fmt.Scanln(&confirmation); err != nil {
		return err
	}
	if confirmation == "y" {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Deleted %q\n", showpath)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Did not delete %q\n", showpath)
	return nil
}

// findDb: returns the path to the named db or an errDBNotFound if no
// match is found.
func findDb(name string) (string, error) {
	sName, err := nameFromArgs([]string{name})
	if err != nil {
		return "", err
	}
	path, err := getFilePath(sName)
	if err != nil {
		return "", err
	}
	_, err = os.Stat(path)
	if sName == "" || os.IsNotExist(err) {
		dbs, err := getDbs()
		if err != nil {
			return "", err
		}
		var suggestions []string
		for _, db := range dbs {
			diff := int(math.Abs(float64(len(db) - len(name))))
			levenshteinDistance := levenshtein.ComputeDistance(name, db)
			suggestByLevenshtein := levenshteinDistance <= diff
			if suggestByLevenshtein {
				suggestions = append(suggestions, db)
			}
		}
		return "", errDBNotFound{suggestions: suggestions}
	}
	return path, nil
}

//nolint:wrapcheck
func list(_ *cobra.Command, args []string) error {
	var k string
	var pf string
	if keysIterate || valuesIterate {
		pf = "%s\n"
	} else {
		var err error
		pf, err = strconv.Unquote(fmt.Sprintf(`"%%s%s%%s\n"`, delimiterIterate))
		if err != nil {
			return err
		}
	}
	if len(args) == 1 {
		k = args[0]
		if !strings.Contains(k, "@") {
			k = "@" + k
		}
	}
	_, n, err := keyParser(k)
	if err != nil {
		return err
	}
	db, err := openKV(n)
	if err != nil {
		return err
	}
	err = db.Sync()
	if err != nil {
		return err
	}
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		opts.Reverse = reverseIterate
		if keysIterate {
			opts.PrefetchValues = false
		}
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()
			if keysIterate {
				printFromKV(pf, k)
				continue
			}
			err := item.Value(func(v []byte) error {
				if valuesIterate {
					printFromKV(pf, v)
				} else {
					printFromKV(pf, k, v)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func nameFromArgs(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	_, n, err := keyParser(args[0])
	if err != nil {
		return "", err
	}
	return n, nil
}

func printFromKV(pf string, vs ...[]byte) {
	nb := "(omitted binary data)"
	fvs := make([]any, 0)
	isatty := term.IsTerminal(int(os.Stdin.Fd()))
	for _, v := range vs {
		if isatty && !showBinary && !utf8.Valid(v) {
			fvs = append(fvs, nb)
		} else {
			fvs = append(fvs, string(v))
		}
	}
	fmt.Printf(pf, fvs...)
	if isatty && !strings.HasSuffix(pf, "\n") {
		fmt.Println()
	}
}

func keyParser(k string) ([]byte, string, error) {
	var key, db string
	ps := strings.Split(k, "@")
	switch len(ps) {
	case 1:
		key = strings.ToLower(ps[0])
	case 2:
		key = strings.ToLower(ps[0])
		db = strings.ToLower(ps[1])
	default:
		return nil, "", fmt.Errorf("bad key format, use KEY@DB")
	}
	return []byte(key), db, nil
}

func openKV(name string) (*badger.DB, error) {
	if name == "" {
		name = "default"
	}
	path, err := getFilePath(name)
	if err != nil {
		return nil, err
	}
	return badger.Open(badger.DefaultOptions(path).WithLoggingLevel(badger.ERROR)) //nolint:wrapcheck
}

func getCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		if strings.Contains(toComplete, "@") {
			parts := strings.SplitN(toComplete, "@", 2)
			keyPrefix := parts[0]
			dbs, err := getRawDbs()
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			var completions []string
			for _, db := range dbs {
				completions = append(completions, keyPrefix+"@"+db)
			}
			return completions, cobra.ShellCompDirectiveNoFileComp
		}

		dbs, err := getRawDbs()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var completions []string
		completions = append(completions, dbs...)
		defaultKeys, err := getKeysInDb("")
		if err == nil {
			completions = append(completions, defaultKeys...)
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}

	if len(args) == 1 {
		var dbName string
		isDb := false
		if strings.HasPrefix(args[0], "@") {
			dbName = strings.TrimPrefix(args[0], "@")
			isDb = true
		} else {
			dbs, err := getRawDbs()
			if err == nil {
				for _, d := range dbs {
					if strings.EqualFold(d, args[0]) {
						dbName = d
						isDb = true
						break
					}
				}
			}
		}
		if isDb {
			keys, err := getKeysInDb(dbName)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return keys, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return nil, cobra.ShellCompDirectiveNoFileComp
}

func setCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		if strings.Contains(toComplete, "@") {
			parts := strings.SplitN(toComplete, "@", 2)
			keyPrefix := parts[0]
			dbs, err := getRawDbs()
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			var completions []string
			for _, db := range dbs {
				completions = append(completions, keyPrefix+"@"+db)
			}
			return completions, cobra.ShellCompDirectiveNoFileComp
		}

		dbs, err := getRawDbs()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var completions []string
		completions = append(completions, dbs...)
		defaultKeys, err := getKeysInDb("")
		if err == nil {
			completions = append(completions, defaultKeys...)
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}

	if len(args) == 1 {
		var dbName string
		isDb := false
		if strings.HasPrefix(args[0], "@") {
			dbName = strings.TrimPrefix(args[0], "@")
			isDb = true
		} else {
			dbs, err := getRawDbs()
			if err == nil {
				for _, d := range dbs {
					if strings.EqualFold(d, args[0]) {
						dbName = d
						isDb = true
						break
					}
				}
			}
		}
		if isDb {
			keys, err := getKeysInDb(dbName)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return keys, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return nil, cobra.ShellCompDirectiveNoFileComp
}

func deleteCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return setCompletion(cmd, args, toComplete)
}

func listCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		dbs, err := getRawDbs()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var completions []string
		completions = append(completions, dbs...)
		for _, db := range dbs {
			if !contains(completions, "@"+db) {
				completions = append(completions, "@"+db)
			}
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func deleteDbCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return listCompletion(cmd, args, toComplete)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func init() {
	listCmd.Flags().BoolVarP(&reverseIterate, "reverse", "r", false, "list in reverse lexicographic order")
	listCmd.Flags().BoolVarP(&keysIterate, "keys-only", "k", false, "only print keys and don't fetch values from the db")
	listCmd.Flags().BoolVarP(&valuesIterate, "values-only", "v", false, "only print values")
	listCmd.Flags().StringVarP(&delimiterIterate, "delimiter", "d", "\t", "delimiter to separate keys and values")
	listCmd.Flags().BoolVarP(&showBinary, "show-binary", "b", false, "print binary values")
	getCmd.Flags().BoolVarP(&showBinary, "show-binary", "b", false, "print binary values")

	getCmd.ValidArgsFunction = getCompletion
	setCmd.ValidArgsFunction = setCompletion
	deleteCmd.ValidArgsFunction = deleteCompletion
	listCmd.ValidArgsFunction = listCompletion
	deleteDbCmd.ValidArgsFunction = deleteDbCompletion

	rootCmd.AddCommand(
		getCmd,
		setCmd,
		deleteCmd,
		listCmd,
		listDbsCmd,
		deleteDbCmd,
		completionCmd,
	)
}

func main() {
	if err := fang.Execute(context.Background(), rootCmd); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
}

func wrap(db *badger.DB, readonly bool, fn func(tx *badger.Txn) error) error {
	tx := db.NewTransaction(!readonly)
	if err := fn(tx); err != nil {
		tx.Discard()
		return err
	}
	return tx.Commit() //nolint:wrapcheck
}
