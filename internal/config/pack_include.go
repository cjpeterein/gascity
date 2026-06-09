package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

var runRepoCacheGit = defaultRunRepoCacheGit

// includeCacheDir is the subdirectory under .gc/cache/includes/ where
// remote pack includes are cached.
const includeCacheDir = citylayout.CacheIncludesRoot

// isRemoteInclude reports whether s is a remote include URL.
func isRemoteInclude(s string) bool {
	return remotesource.IsRemote(s)
}

// parseRemoteInclude splits a remote include string into source, subpath,
// and ref components. Format: <source>//<subpath>#<ref>
// Both //subpath and #ref are optional.
//
// Examples:
//
//	"git@github.com:org/repo.git//topo#v1.0" → ("git@github.com:org/repo.git", "topo", "v1.0")
//	"https://github.com/org/repo.git#main"   → ("https://github.com/org/repo.git", "", "main")
//	"git@github.com:org/repo.git"            → ("git@github.com:org/repo.git", "", "")
func parseRemoteInclude(s string) (source, subpath, ref string) {
	parsed := remotesource.Parse(s)
	return parsed.CloneURL, parsed.Subpath, parsed.Ref
}

// includeCacheName returns a deterministic, human-readable cache directory
// name for a remote include source URL. Format: <slug>-<sha256[:12]>.
// Slug is the last path component of the URL with .git stripped.
func includeCacheName(source string) string {
	// Extract slug: last path component, strip .git suffix.
	slug := source
	// For SSH URLs like git@github.com:org/repo.git, use the part after ':'
	if i := strings.LastIndex(slug, ":"); i >= 0 && !strings.Contains(slug, "://") {
		slug = slug[i+1:]
	}
	// For all URLs, take the last path component.
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	slug = strings.TrimSuffix(slug, ".git")
	if slug == "" {
		slug = "include"
	}

	// Compute short hash for uniqueness.
	h := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%s-%x", slug, h[:6])
}

// isRemoteRef reports whether s is any kind of remote pack reference
// (remote include URL or GitHub tree URL).
func isRemoteRef(s string) bool {
	return isRemoteInclude(s) || isGitHubTreeURL(s)
}

// isGitHubTreeURL reports whether s looks like a GitHub tree or blob URL.
// GitHub tree URLs have the format:
//
//	https://github.com/{owner}/{repo}/tree/{ref}[/{path}]
func isGitHubTreeURL(s string) bool {
	return remotesource.IsGitHubTreeOrBlob(s)
}

// parseGitHubTreeURL extracts repo, ref, and subpath from a GitHub tree URL.
//
// Input:  https://github.com/org/repo/tree/v1.0.0/packs/base
// Output: source=https://github.com/org/repo.git, ref=v1.0.0, subpath=packs/base
//
// Limitation: ref is parsed as a single path component. For branches
// with "/" in the name, use the source//subpath#ref format instead.
func parseGitHubTreeURL(s string) (source, subpath, ref string) {
	parsed, ok := remotesource.ParseGitHubTreeOrBlob(s)
	if !ok {
		return s, "", ""
	}
	return parsed.CloneURL, parsed.Subpath, parsed.Ref
}

// resolvePackRef resolves a pack reference to a local directory.
// Handles local paths, GitHub tree URLs, and git source//sub#ref URLs.
func resolvePackRef(ref, declDir, cityRoot string) (string, error) {
	if isGitHubTreeURL(ref) || isRemoteInclude(ref) {
		// parseRemoteInclude handles GitHub tree/blob URLs too
		// (remotesource.Parse short-circuits to ParseGitHubTreeOrBlob),
		// so a single parse covers both remote forms.
		source, subpath, gitRef := parseRemoteInclude(ref)
		// packs.lock is authoritative for any remote source string it
		// records, with or without an embedded ref: gc import install /
		// upgrade already resolved the authored source to a commit and
		// populated the repo cache. Consulting the lock first keeps
		// locked imports (including registry-recommended GitHub tree
		// URLs) resolvable without the legacy include cache, which has
		// no remaining writer.
		if cacheDir, ok, err := resolveLockedRemoteImport(ref, cityRoot); err != nil {
			return "", err
		} else if ok {
			if subpath != "" {
				return filepath.Join(cacheDir, subpath), nil
			}
			return cacheDir, nil
		}
		cacheDir, err := fetchRemoteInclude(source, gitRef, cityRoot)
		if err != nil {
			return "", err
		}
		if subpath != "" {
			return filepath.Join(cacheDir, subpath), nil
		}
		return cacheDir, nil
	}
	return resolveConfigPath(ref, declDir, cityRoot), nil
}

type remoteImportLockfile struct {
	Packs map[string]remoteImportLockEntry `toml:"packs"`
}

type remoteImportLockEntry struct {
	Commit string `toml:"commit"`
}

func resolveLockedRemoteImport(source, cityRoot string) (string, bool, error) {
	return resolveRemoteImportCacheDir(source, cityRoot)
}

func resolveInstalledRemoteImport(source, cityRoot string) (string, error) {
	cacheDir, ok, err := resolveRemoteImportCacheDir(source, cityRoot)
	if err != nil {
		return "", err
	}
	if !ok {
		// Distinguish "no packs.lock" from "no entry for this source" so the
		// remediation hint is precise. This re-stat runs only on the error
		// path, so it never adds to the hot reconcile cost.
		lockPath := filepath.Join(cityRoot, "packs.lock")
		if _, statErr := os.Stat(lockPath); errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("remote import %s is not installed (missing packs.lock); run \"gc import install\"", source)
		}
		return "", fmt.Errorf("remote import %s is not installed (missing packs.lock entry); run \"gc import install\"", source)
	}
	return cacheDir, nil
}

// remoteImportResolutionCache memoizes (source, cityRoot, packs.lock
// fingerprint) -> validated cache directory. Config load resolves the same
// remote import many times per gc invocation: the city pack graph, every rig
// graph, the default-rig graph, the revision hash, and the provenance snapshot
// each walk imports with their own dedup maps, and the whole LoadWithIncludes
// pipeline runs several times per command. The validation memo
// (remoteCacheValidationCache, gc-0kc) already collapses the git execs, but
// each resolution still re-reads and re-parses packs.lock, re-stats the cache
// for a fingerprint, and re-enters the validation memo — 18-27x per single gc
// invocation (gc-oae). Memoizing the resolved cacheDir collapses those passes
// so each distinct import resolves O(1) times per process.
//
// Keyed on the packs.lock fingerprint (size+mtime) so an upgrade or repair
// that rewrites the lockfile is picked up within the same process. Otherwise
// the cache is a pinned, gc-managed checkout that only "gc import install"
// rewrites — and install always rewrites packs.lock — so a warm entry is safe
// to reuse without re-validating. Only successful resolutions are cached;
// errors and "not installed" results re-check so a repaired cache or a freshly
// installed import is picked up immediately.
var remoteImportResolutionCache sync.Map // source+"\x00"+cityRoot+"\x00"+lockfp -> string (cacheDir)

// packsLockFingerprint returns a cheap change signal (size+mtime) for the
// packs.lock at lockPath, or "-" when it is absent or unreadable. A missing
// lockfile is never a cache hit, so the sentinel only has to be stable.
func packsLockFingerprint(lockPath string) string {
	if fi, err := os.Stat(lockPath); err == nil {
		return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}
	return "-"
}

// resolveRemoteImportCacheDir resolves a locked remote import source to its
// validated cache directory, memoized per process (see
// remoteImportResolutionCache). ok is false with a nil error when packs.lock is
// absent or has no entry for source.
func resolveRemoteImportCacheDir(source, cityRoot string) (string, bool, error) {
	lockPath := filepath.Join(cityRoot, "packs.lock")
	key := source + "\x00" + cityRoot + "\x00" + packsLockFingerprint(lockPath)
	if v, ok := remoteImportResolutionCache.Load(key); ok {
		return v.(string), true, nil
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading packs.lock: %w", err)
	}

	var lock remoteImportLockfile
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return "", false, fmt.Errorf("parsing packs.lock: %w", err)
	}
	entry, ok := lock.Packs[source]
	if !ok || entry.Commit == "" {
		return "", false, nil
	}

	cacheRoot, err := RepoCacheRoot()
	if err != nil {
		return "", false, err
	}
	cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, entry.Commit))
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, entry.Commit); err != nil {
		return "", false, err
	}
	remoteImportResolutionCache.Store(key, cacheDir)
	return cacheDir, true, nil
}

// remoteCacheValidationCache memoizes successful remote-cache validations. The
// installed remote pack cache is a commit-pinned, gc-managed checkout: validating
// it costs a repo-cache flock plus two git execs (`rev-parse HEAD` and
// `status --porcelain --ignored`, the latter walking the whole tree), and config
// load runs it for every remote import on every reconcile (per rig, per pool).
// Since the cache is immutable for a given commit unless `gc import install`
// rewrites it, cache the success keyed by (cacheDir, commit) + a cheap stat
// fingerprint so a warm cache is revalidated once and reused. Only successes are
// cached; an error re-checks so a repaired cache is picked up immediately.
var remoteCacheValidationCache sync.Map // cacheDir+"\x00"+commit -> remoteCacheValidationEntry

type remoteCacheValidationEntry struct{ fingerprint string }

// remoteCacheFingerprint is a cheap change signal for a remote cache checkout.
// It must move when `gc import install` rewrites the cache but stay constant
// across the `git status` that validation itself runs — otherwise the memo
// self-invalidates and every config load re-execs git (gc-0kc).
//
// `git status` perturbs two signals that a naive fingerprint would trip over:
//   - the `.git` directory mtime, bumped by `index.lock` create/unlink, and
//   - the `.git/index` mtime, bumped by git refreshing the index stat-cache
//     (racy-clean protection) on a freshly checked-out tree.
//
// So neither the `.git` directory nor the index mtime can be trusted. The
// fingerprint instead keys on signals `git status` never writes but install
// always does: the checkout root (size+mtime), `.git/HEAD` (size+mtime — every
// install re-checkout rewrites it), and the index *size* (mtime excluded). A
// nested manual worktree edit touching none of these escapes detection until
// the process restarts — acceptable for a pinned, gc-managed cache.
func remoteCacheFingerprint(cacheDir string) string {
	var b strings.Builder
	statBoth := func(p string) {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d:%d;", fi.Size(), fi.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	statBoth(cacheDir)
	statBoth(filepath.Join(cacheDir, ".git", "HEAD"))
	if fi, err := os.Stat(filepath.Join(cacheDir, ".git", "index")); err == nil {
		fmt.Fprintf(&b, "%d;", fi.Size())
	} else {
		b.WriteString("-;")
	}
	return b.String()
}

// validateInstalledRemoteCacheLocked validates the remote cache under the
// repo-cache read lock, memoizing the success so a warm, unchanged cache skips
// both the flock and the git execs on subsequent loads.
func validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit string) error {
	key := cacheDir + "\x00" + commit
	fp := remoteCacheFingerprint(cacheDir)
	if v, ok := remoteCacheValidationCache.Load(key); ok {
		if v.(remoteCacheValidationEntry).fingerprint == fp {
			return nil
		}
	}
	if err := WithRepoCacheReadLock(cacheRoot, func() error {
		return validateInstalledRemoteCache(source, cacheDir, commit)
	}); err != nil {
		// A synthetic bundled-pack cache is a deterministic projection of the
		// running binary's embedded packs, so two faults are rebuildable
		// in-process with no network: a content-hash mismatch (the binary was
		// rebuilt) and an absent cache (cold or isolated cache root). Self-heal
		// those instead of bricking every read-only command until a human runs
		// "gc import install". Tamper and corruption failures (bad schema,
		// content drift, repo/commit mismatch) are neither sentinel and still
		// hard-fail; remote git imports have no offline rebuild path and also
		// keep their hard failure.
		if !errors.Is(err, builtinpacks.ErrSyntheticContentHashMismatch) &&
			!errors.Is(err, builtinpacks.ErrSyntheticCacheMissing) {
			return err
		}
		if healErr := selfHealSyntheticCacheLocked(source, cacheRoot, cacheDir, commit); healErr != nil {
			return healErr
		}
		fp = remoteCacheFingerprint(cacheDir)
	}
	remoteCacheValidationCache.Store(key, remoteCacheValidationEntry{fingerprint: fp})
	return nil
}

// selfHealSyntheticCacheLocked rematerializes a synthetic bundled-pack cache
// under the repo-cache write lock. The read-lock validation already failed, so
// this drops to the exclusive lock, re-validates (another process may have
// healed the cache in the gap between unlocking and relocking), and only
// rebuilds the cache if it is still invalid. Mirrors packman's
// ensureBundledRepoInCacheLocked self-heal so both code paths share the same
// recovery contract.
func selfHealSyntheticCacheLocked(source, cacheRoot, cacheDir, commit string) error {
	_, err := WithRepoCacheWriteLock(cacheRoot, func() (string, error) {
		if err := builtinpacks.ValidateSyntheticRepo(cacheDir, commit); err == nil {
			return "", nil
		}
		if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
			return "", fmt.Errorf("rematerializing synthetic cache for %s at %s: %w", source, cacheDir, err)
		}
		if err := builtinpacks.ValidateSyntheticRepo(cacheDir, commit); err != nil {
			return "", fmt.Errorf("validating rematerialized synthetic cache for %s at %s: %w", source, cacheDir, err)
		}
		return "", nil
	})
	return err
}

// ResetRemoteCacheValidationCache clears memoized remote-cache validations and
// resolutions (test isolation; also lets `gc import install` force
// revalidation in-process).
func ResetRemoteCacheValidationCache() {
	remoteCacheValidationCache.Range(func(k, _ any) bool {
		remoteCacheValidationCache.Delete(k)
		return true
	})
	remoteImportResolutionCache.Range(func(k, _ any) bool {
		remoteImportResolutionCache.Delete(k)
		return true
	})
}

func validateInstalledRemoteCache(source, cacheDir, commit string) error {
	gitPath := filepath.Join(cacheDir, ".git")
	gitInfo, gitStatErr := os.Stat(gitPath)
	if builtinpacks.IsSource(source) {
		err := builtinpacks.ValidateSyntheticRepo(cacheDir, commit)
		if err == nil {
			return nil
		}
		if gitutil.MissingCheckoutMarker(gitInfo, gitStatErr) {
			return fmt.Errorf("remote import %s is locked but synthetic cache is invalid at %s: %w; run \"gc import install\"", source, cacheDir, err)
		}
		if gitStatErr != nil {
			return fmt.Errorf("checking cached import %s: %w; synthetic cache is invalid at %s: %w", source, gitStatErr, cacheDir, err)
		}
		// Synthetic cache is invalid but a real git checkout exists at this
		// path, so validate it with the ordinary remote-cache contract below.
	}
	if gitutil.MissingCheckoutMarker(gitInfo, gitStatErr) {
		return fmt.Errorf("remote import %s is locked but not cached at %s; run \"gc import install\"", source, cacheDir)
	}
	if gitStatErr != nil {
		return fmt.Errorf("checking cached import %s: %w", source, gitStatErr)
	}
	if err := validateLockedRemoteCache(source, cacheDir, commit); err != nil {
		return err
	}
	return nil
}

func validateLockedRemoteCache(source, cacheDir, commit string) error {
	head, err := runRepoCacheGit(cacheDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading cached import %s HEAD: %w", source, err)
	}
	if !gitutil.SameCommit(head, commit) {
		return fmt.Errorf("cached import %s is checked out at %s, expected %s; run \"gc import install\"", source, strings.TrimSpace(head), commit)
	}
	status, err := runRepoCacheGit(cacheDir, "status", "--porcelain", "--ignored")
	if err != nil {
		return fmt.Errorf("checking cached import %s worktree status: %w", source, err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("cached import %s has local worktree changes; run \"gc import install\"", source)
	}
	return nil
}

func defaultRunRepoCacheGit(dir string, args ...string) (string, error) {
	cmdArgs := append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && repoCacheGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

var repoCacheGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_NOSYSTEM":              true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_EXEC_PATH":                    true,
	"GIT_PAGER":                        true,
}

// RepoCacheKey computes the sha256 cache key for a remote source+commit pair.
// This is the canonical implementation — packman.RepoCacheKey must produce
// identical results. Bundled synthetic caches live in a distinct namespace so
// current-binary content never collides with same-repo git checkouts, and they
// additionally fold in the running binary's bundled-pack content hash so two gc
// binaries with different embedded pack content resolve to different cache
// directories instead of fighting over one shared marker (the citywide
// "bundled pack cache content hash does not match current binary" wedge).
func RepoCacheKey(source, commit string) string {
	identity := NormalizeRemoteSource(source) + commit
	if builtinpacks.IsSource(source) {
		identity = builtinpacks.SyntheticCacheNamespace + "\x00" + NormalizeRemoteSource(source) + "\x00" + commit
		if component := builtinpacks.SyntheticCacheKeyComponent(); component != "" {
			identity += "\x00" + component
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", sum[:])
}

// RepoCacheRootEnv is the environment variable that overrides the machine-local
// repo cache root. Test harnesses that must keep the real HOME (e.g. the
// platform supervisor validates HOME == OS user home) set this to a temp dir so
// shelling `gc` never stamps the shared synthetic-pack marker into the live
// ~/.gc/cache/repos.
const RepoCacheRootEnv = "GC_REPO_CACHE_ROOT"

// RepoCacheRoot returns the canonical machine-local repo cache root.
//
// Resolution order:
//  1. GC_REPO_CACHE_ROOT (dedicated isolation override)
//  2. $GC_HOME/cache/repos (matches gchome.Default and the lock-root candidates)
//  3. $HOME/.gc/cache/repos (default)
//
// This is the single source of truth for the cache root. Every reader and
// writer (packman materialize/install and config import validation) must route
// through it so they never disagree on where the cache lives.
func RepoCacheRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv(RepoCacheRootEnv)); override != "" {
		return override, nil
	}
	if gcHome := strings.TrimSpace(os.Getenv("GC_HOME")); gcHome != "" {
		return filepath.Join(gcHome, "cache", "repos"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".gc", "cache", "repos"), nil
}

// NormalizeRemoteSource extracts the clone URL from a source string,
// stripping subpath and ref suffixes. This is the canonical normalization
// for cache key computation — packman must use the same logic.
func NormalizeRemoteSource(source string) string {
	if !isRemoteRef(source) {
		return source
	}
	return remotesource.Parse(source).CloneURL
}

// fetchRemoteInclude resolves a remote pack include from the local cache.
// The loader is a pure reader: git operations must happen ahead of time.
// Cache location: <cityRoot>/.gc/cache/includes/<cache-name>/
func fetchRemoteInclude(source, ref, cityRoot string) (string, error) {
	cacheName := includeCacheName(source)
	cacheDir := filepath.Join(cityRoot, includeCacheDir, cacheName)

	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); err != nil {
		if os.IsNotExist(err) {
			if ref != "" {
				return "", fmt.Errorf("remote include %s#%s is not cached at %s", source, ref, cacheDir)
			}
			return "", fmt.Errorf("remote include %s is not cached at %s", source, cacheDir)
		}
		return "", fmt.Errorf("checking cached include %s: %w", source, err)
	}

	return cacheDir, nil
}
