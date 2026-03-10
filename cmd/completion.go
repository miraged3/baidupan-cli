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

	displayBase, baseDir, namePrefix := splitRemotePathForCompletion(prefix)
	files, err := listRemoteDirCached(baseDir)
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(files))

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
		out = append(out, buildCompletionCandidate(displayBase, f.ServerFilename, f.IsDir == 1))
	}

	if len(out) == 0 && baseDir == "/" && namePrefix == "" {
		return []string{"/"}
	}

	sort.Strings(out)
	return dedupeStrings(out)
}

func splitRemotePathForCompletion(prefix string) (displayBase, baseDir, namePrefix string) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		if app.CurrentDir == "/" {
			return "/", "/", ""
		}
		return ensureTrailingSlash(app.CurrentDir), app.CurrentDir, ""
	}

	resolvedPrefix := ResolvePath(prefix)
	if strings.HasSuffix(prefix, "/") {
		return prefix, resolvedPrefix, ""
	}

	baseDir = pathpkg.Dir(resolvedPrefix)
	if baseDir == "." {
		baseDir = "/"
	}

	displayBase = pathpkg.Dir(prefix)
	if displayBase == "." {
		displayBase = ""
	} else if displayBase != "/" {
		displayBase = ensureTrailingSlash(displayBase)
	} else {
		displayBase = "/"
	}
	return displayBase, baseDir, pathpkg.Base(prefix)
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

func buildCompletionCandidate(displayBase, name string, isDir bool) string {
	candidate := displayBase + name
	if displayBase == "/" {
		candidate = "/" + name
	}
	if displayBase == "" {
		candidate = name
	}
	if isDir {
		candidate = ensureTrailingSlash(candidate)
	}
	return candidate
}

func ensureTrailingSlash(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
}
