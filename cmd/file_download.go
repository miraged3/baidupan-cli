package cmd

import (
	"baidupan-cli/app"
	"baidupan-cli/util"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/desertbit/grumble"
)

type fileMetaResp struct {
	BaseVo
	List []*downloadFileMeta `json:"list,omitempty"`
}

type downloadFileMeta struct {
	FsID     uint64 `json:"fs_id,omitempty"`
	Path     string `json:"path,omitempty"`
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
	IsDir    int    `json:"isdir,omitempty"`
	Dlink    string `json:"dlink,omitempty"`
}

var fileDownloadCmd = &grumble.Command{
	Name:    "download",
	Aliases: []string{"dl"},
	Help:    "download a remote file to local filesystem",
	Usage:   "download REMOTE_PATH [LOCAL_PATH]",
	Args: func(a *grumble.Args) {
		a.String("remote", "remote file path")
		a.String("local", "local file path (optional)", grumble.Default(""))
	},
	Flags: func(f *grumble.Flags) {
		f.String("o", "output", "", "local output path (overrides positional LOCAL_PATH)")
		f.Bool("f", "force", false, "overwrite local file if it already exists")
		f.Int("t", "timeout", 0, "download timeout in seconds (0 means no timeout)")
	},
	Run: func(ctx *grumble.Context) error {
		if err := checkAuthorized(ctx); err != nil {
			return err
		}

		remotePath := ResolvePath(ctx.Args.String("remote"))
		if remotePath == "/" {
			return fmt.Errorf("remote path must be a file, but got '/'")
		}

		localPath := strings.TrimSpace(ctx.Flags.String("output"))
		if localPath == "" {
			localPath = strings.TrimSpace(ctx.Args.String("local"))
		}

		meta, err := lookupDownloadMeta(remotePath)
		if err != nil {
			return err
		}
		if meta.IsDir != 0 {
			return fmt.Errorf("directory download is not supported yet: %s", remotePath)
		}
		if strings.TrimSpace(meta.Dlink) == "" {
			return fmt.Errorf("download link is empty for %s", remotePath)
		}

		localPath, err = resolveLocalDownloadPath(localPath, meta.Filename)
		if err != nil {
			return err
		}
		if err := ensureLocalDownloadTarget(localPath, ctx.Flags.Bool("force")); err != nil {
			return err
		}

		timeout := time.Duration(ctx.Flags.Int("timeout")) * time.Second
		fmt.Printf("downloading %s -> %s\n", remotePath, localPath)
		if err := downloadToLocal(meta, localPath, timeout); err != nil {
			return err
		}
		fmt.Printf("downloaded %s (%s)\n", localPath, util.ConvReadableSize(meta.Size))
		return nil
	},
}

func lookupDownloadMeta(remotePath string) (*downloadFileMeta, error) {
	file, err := findRemoteFile(remotePath)
	if err != nil {
		return nil, err
	}

	respStr, _, err := app.APIClient.MultimediafileApi.Xpanmultimediafilemetas(RootContext).
		AccessToken(*TokenResp.AccessToken).
		Fsids(fmt.Sprintf("[%d]", file.FsId)).
		Dlink("1").
		Execute()
	if err != nil {
		return nil, err
	}

	var resp fileMetaResp
	if err := sonic.UnmarshalString(respStr, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse file metas response: %w, raw=%s", err, respStr)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("filemetas error code: %d", resp.Errno)
	}
	if len(resp.List) == 0 || resp.List[0] == nil {
		return nil, fmt.Errorf("file metadata not found for %s", remotePath)
	}
	meta := resp.List[0]
	if strings.TrimSpace(meta.Path) == "" {
		meta.Path = remotePath
	}
	if strings.TrimSpace(meta.Filename) == "" {
		meta.Filename = pathpkg.Base(remotePath)
	}
	return meta, nil
}

func findRemoteFile(remotePath string) (*File, error) {
	parentDir := pathpkg.Dir(remotePath)
	if parentDir == "." {
		parentDir = "/"
	}
	targetName := pathpkg.Base(remotePath)
	start := int32(0)
	limit := int32(1000)

	for {
		req := app.APIClient.FileinfoApi.Xpanfilelist(RootContext).
			AccessToken(*TokenResp.AccessToken).
			Dir(parentDir).
			Start(strconv.FormatInt(int64(start), 10)).
			Limit(limit).
			Order(orderByName)

		respStr, _, err := req.Execute()
		if err != nil {
			return nil, err
		}

		var resp FileListResp
		if err := sonic.UnmarshalString(respStr, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse list response: %w, raw=%s", err, respStr)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("list error code: %d", resp.Errno)
		}

		for _, f := range resp.Files {
			if f == nil {
				continue
			}
			if f.ServerFilename == targetName && f.Path == remotePath {
				return f, nil
			}
		}

		if len(resp.Files) < int(limit) {
			break
		}
		start += limit
	}

	return nil, fmt.Errorf("remote file not found: %s", remotePath)
}

func resolveLocalDownloadPath(localPath, filename string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", fmt.Errorf("empty remote filename")
	}
	if strings.TrimSpace(localPath) == "" {
		return filepath.Abs(filename)
	}

	if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		return filepath.Join(localPath, filename), nil
	}

	if strings.HasSuffix(localPath, string(os.PathSeparator)) {
		return filepath.Join(localPath, filename), nil
	}

	return filepath.Abs(localPath)
}

func ensureLocalDownloadTarget(localPath string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	info, err := os.Stat(localPath)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("local path is a directory: %s", localPath)
		}
		if !force {
			return fmt.Errorf("local file already exists: %s (use --force to overwrite)", localPath)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check local path: %w", err)
	}
	return nil
}

func downloadToLocal(meta *downloadFileMeta, localPath string, timeout time.Duration) error {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	downloadURL, err := buildDownloadURL(meta.Dlink)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	req.Host = "d.pcs.baidu.com"

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download failed: status=%s body=%s", resp.Status, strings.TrimSpace(string(body)))
	}

	tmpPath := localPath + ".part"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	pw := &progressWriter{
		name:  meta.Filename,
		total: resp.ContentLength,
	}

	_, copyErr := io.Copy(out, io.TeeReader(resp.Body, pw))
	if copyErr != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return copyErr
	}
	pw.finish()

	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if _, err := os.Stat(localPath); err == nil {
		if err := os.Remove(localPath); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func buildDownloadURL(rawDlink string) (string, error) {
	if TokenResp == nil || TokenResp.AccessToken == nil || strings.TrimSpace(*TokenResp.AccessToken) == "" {
		return "", fmt.Errorf("missing access token")
	}

	u, err := url.Parse(rawDlink)
	if err != nil {
		return "", fmt.Errorf("invalid dlink: %w", err)
	}
	q := u.Query()
	q.Set("access_token", strings.TrimSpace(*TokenResp.AccessToken))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type progressWriter struct {
	name       string
	total      int64
	written    int64
	lastReport time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	now := time.Now()
	if w.lastReport.IsZero() || now.Sub(w.lastReport) >= 500*time.Millisecond {
		w.printProgress()
		w.lastReport = now
	}
	return n, nil
}

func (w *progressWriter) finish() {
	w.printProgress()
	fmt.Println()
}

func (w *progressWriter) printProgress() {
	if w.total > 0 {
		percent := float64(w.written) / float64(w.total) * 100
		fmt.Printf("\r%s  %s / %s (%.1f%%)", w.name, util.ConvReadableSize(w.written), util.ConvReadableSize(w.total), percent)
		return
	}
	fmt.Printf("\r%s  %s", w.name, util.ConvReadableSize(w.written))
}
