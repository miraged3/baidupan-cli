package cmd

import (
	"baidupan-cli/app"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
)

type remotePathCompleterConfig struct {
	OnlyDirs          bool
	Positionals       int
	FlagValueMatchers map[string]bool
	Flags             []string
}

type remoteListCacheEntry struct {
	files     []*File
	expiresAt time.Time
}

var (
	remoteListCacheMu sync.Mutex
	remoteListCache   = map[string]remoteListCacheEntry{}
)

func remotePathCompleter(cfg remotePathCompleterConfig) func(prefix string, args []string) []string {
	return func(prefix string, args []string) []string {
		if strings.HasPrefix(prefix, "-") {
			return filterCompletions(prefix, cfg.Flags)
		}

		if isCompletingFlagValue(args) {
			if shouldCompleteRemoteFlagValue(args, cfg.FlagValueMatchers) {
				return completeRemotePath(prefix, cfg.OnlyDirs)
			}
			return nil
		}

		if shouldCompleteRemoteFlagValue(args, cfg.FlagValueMatchers) {
			return completeRemotePath(prefix, cfg.OnlyDirs)
		}

		if countPositionalArgs(args) >= cfg.Positionals {
			return nil
		}

		return completeRemotePath(prefix, cfg.OnlyDirs)
	}
}

func isCompletingFlagValue(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return flagExpectsValue(strings.TrimSpace(args[len(args)-1]))
}

func shouldCompleteRemoteFlagValue(args []string, matchers map[string]bool) bool {
	if len(args) == 0 || len(matchers) == 0 {
		return false
	}
	last := strings.TrimSpace(args[len(args)-1])
	return matchers[last]
}

func countPositionalArgs(args []string) int {
	count := 0
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}

		if strings.TrimSpace(arg) == "" {
			continue
		}
		if arg == "--" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if flagExpectsValue(arg) {
				skipNext = true
			}
			continue
		}
		count++
	}
	return count
}

func flagExpectsValue(flag string) bool {
	switch flag {
	case "-d", "--dir", "-o", "--output", "-t", "--timeout":
		return true
	default:
		return false
	}
}

func completeRemotePath(prefix string, onlyDirs bool) []string {
	if TokenResp == nil || TokenResp.AccessToken == nil || strings.TrimSpace(*TokenResp.AccessToken) == "" {
		return nil
	}

	resolvedPrefix, baseDir, namePrefix := splitRemotePathForCompletion(prefix)
	files, err := listRemoteDirCached(baseDir)
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(files)+1)
	if baseDir != "/" && "." != namePrefix && strings.HasPrefix(".", namePrefix) {
		parent := pathpkg.Dir(strings.TrimRight(baseDir, "/"))
		if parent == "." {
			parent = "/"
		}
		out = append(out, formatRemoteCompletion(parent, true))
	}

	for _, f := range files {
		if f == nil {
			continue
		}
		if onlyDirs && f.IsDir != 1 {
			continue
		}
		if !strings.HasPrefix(f.ServerFilename, namePrefix) {
			continue
		}
		candidate := f.Path
		if f.IsDir == 1 {
			candidate += "/"
		}
		out = append(out, escapeCompletion(candidate))
	}

	if len(out) == 0 && resolvedPrefix == "/" && namePrefix == "" {
		return []string{"/"}
	}

	sort.Strings(out)
	return dedupeStrings(out)
}

func splitRemotePathForCompletion(prefix string) (resolvedPrefix, baseDir, namePrefix string) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return app.CurrentDir, app.CurrentDir, ""
	}

	resolvedPrefix = ResolvePath(prefix)
	if strings.HasSuffix(prefix, "/") {
		return resolvedPrefix, resolvedPrefix, ""
	}

	baseDir = pathpkg.Dir(resolvedPrefix)
	if baseDir == "." {
		baseDir = "/"
	}
	return resolvedPrefix, baseDir, pathpkg.Base(resolvedPrefix)
}

func listRemoteDirCached(dir string) ([]*File, error) {
	remoteListCacheMu.Lock()
	entry, ok := remoteListCache[dir]
	if ok && time.Now().Before(entry.expiresAt) {
		files := entry.files
		remoteListCacheMu.Unlock()
		return files, nil
	}
	remoteListCacheMu.Unlock()

	req := app.APIClient.FileinfoApi.Xpanfilelist(RootContext).
		AccessToken(*TokenResp.AccessToken).
		Dir(dir).
		Limit(1000).
		Order(orderByName)

	respStr, _, err := req.Execute()
	if err != nil {
		return nil, err
	}

	var resp FileListResp
	if err := sonic.UnmarshalString(respStr, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse completion list response: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("completion list error code: %d", resp.Errno)
	}

	remoteListCacheMu.Lock()
	remoteListCache[dir] = remoteListCacheEntry{
		files:     resp.Files,
		expiresAt: time.Now().Add(2 * time.Second),
	}
	remoteListCacheMu.Unlock()
	return resp.Files, nil
}

func filterCompletions(prefix string, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			out = append(out, candidate)
		}
	}
	return out
}

func dedupeStrings(items []string) []string {
	if len(items) < 2 {
		return items
	}
	out := items[:0]
	var prev string
	for i, item := range items {
		if i == 0 || item != prev {
			out = append(out, item)
			prev = item
		}
	}
	return out
}

func formatRemoteCompletion(path string, isDir bool) string {
	if isDir && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return escapeCompletion(path)
}

func escapeCompletion(s string) string {
	replacer := strings.NewReplacer(
		` `, `\ `,
		`\`, `\\`,
		`"`, `\"`,
		`'`, `\'`,
		"\t", `\t`,
	)
	return replacer.Replace(s)
}
