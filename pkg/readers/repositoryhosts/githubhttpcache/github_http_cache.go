// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package githubhttpcache

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate -header ../../../../license_prefix.txt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gardener/docforge/pkg/manifest"
	"github.com/gardener/docforge/pkg/osfakes/httpclient"
	"github.com/gardener/docforge/pkg/osfakes/osshim"
	"github.com/gardener/docforge/pkg/readers/link"
	"github.com/gardener/docforge/pkg/readers/repositoryhosts"
	"github.com/google/go-github/v43/github"
	"k8s.io/klog/v2"
)

// GHC implements repositoryhosts.RepositoryHost interface using GitHub manifestadapter with transport level persistent cache.
type GHC struct {
	hostName      string
	client        httpclient.Client
	git           Git
	rateLimit     RateLimitSource
	repositories  Repositories
	os            osshim.Os
	acceptedHosts []string
	localMappings map[string]string
	filesCache    map[string]string
	muxSHA        sync.RWMutex
	defBranches   map[string]string
	muxDefBr      sync.Mutex
	muxCnt        sync.Mutex
	options       manifest.ParsingOptions
}

//counterfeiter:generate . RateLimitSource

// RateLimitSource is an interface needed for faking
type RateLimitSource interface {
	RateLimits(ctx context.Context) (*github.RateLimits, *github.Response, error)
}

//counterfeiter:generate . Repositories

// Repositories is an interface needed for faking
type Repositories interface {
	ListCommits(ctx context.Context, owner, repo string, opts *github.CommitsListOptions) ([]*github.RepositoryCommit, *github.Response, error)
	GetContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (fileContent *github.RepositoryContent, directoryContent []*github.RepositoryContent, resp *github.Response, err error)
	Get(ctx context.Context, owner, repo string) (*github.Repository, *github.Response, error)
}

//counterfeiter:generate . Git

// Git is an interface needed for faking
type Git interface {
	GetBlobRaw(ctx context.Context, owner, repo, sha string) ([]byte, *github.Response, error)
	GetTree(ctx context.Context, owner string, repo string, sha string, recursive bool) (*github.Tree, *github.Response, error)
}

// NewGHC creates new GHC resource handler
func NewGHC(hostName string, rateLimit RateLimitSource, repositories Repositories, git Git, client httpclient.Client, os osshim.Os, acceptedHosts []string, localMappings map[string]string, options manifest.ParsingOptions) repositoryhosts.RepositoryHost {
	return &GHC{
		hostName:      hostName,
		client:        client,
		git:           git,
		rateLimit:     rateLimit,
		repositories:  repositories,
		os:            os,
		acceptedHosts: acceptedHosts,
		localMappings: localMappings,
		filesCache:    make(map[string]string),
		defBranches:   make(map[string]string),
		options:       options,
	}
}

const (
	// DateFormat defines format for LastModifiedDate & PublishDate
	DateFormat = "2006-01-02 15:04:05"
)

// GitInfo defines git resource attributes
type GitInfo struct {
	LastModifiedDate *string        `json:"lastmod,omitempty"`
	PublishDate      *string        `json:"publishdate,omitempty"`
	Author           *github.User   `json:"author,omitempty"`
	Contributors     []*github.User `json:"contributors,omitempty"`
	WebURL           *string        `json:"weburl,omitempty"`
	SHA              *string        `json:"sha,omitempty"`
	SHAAlias         *string        `json:"shaalias,omitempty"`
	Path             *string        `json:"path,omitempty"`
}

//========================= manifest.FileSource ===================================================

// FileTreeFromURL implements manifest.FileSource#FileTreeFromURL
func (p *GHC) FileTreeFromURL(URL string) ([]string, error) {
	r, err := p.getResolvedResourceInfo(context.TODO(), URL)
	if err != nil {
		return nil, err
	}
	if r.Type != "tree" {
		return nil, fmt.Errorf("not a tree url: %s", r.String())
	}
	//bPrefix := fmt.Sprintf("%s://%s/%s/%s/blob/%s/%s", r.URL.Scheme, r.URL.Host, r.Owner, r.Repo, r.Ref, r.Path)
	p.muxSHA.Lock()
	defer p.muxSHA.Unlock()
	if local := p.checkForLocalMapping(r); len(local) > 0 {
		return p.readLocalFileTree(*r, local), nil
	}
	sha := fmt.Sprintf("%s:%s", r.Ref, r.Path)
	sha = url.PathEscape(sha)
	tree, resp, err := p.git.GetTree(context.TODO(), r.Owner, r.Repo, sha, true)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, repositoryhosts.ErrResourceNotFound(r.String())
	}
	if resp != nil && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("reading tree %s fails with HTTP status: %d", r.String(), resp.StatusCode)
	}
	if err != nil {
		return nil, err
	}
	res := []string{}
	for _, e := range tree.Entries {
		extracted := false
		ePath := strings.TrimPrefix(*e.Path, "/")
		for _, extractedFormat := range p.options.ExtractedFilesFormats {
			if strings.HasSuffix(strings.ToLower(ePath), extractedFormat) {
				extracted = true
				break
			}
		}
		// skip node if it is not a supported format
		if *e.Type != "blob" || !extracted {
			//klog.V(6).Infof("node selector %s skip entry %s\n", node.NodeSelector.Path, ePath)
			continue
		}
		res = append(res, ePath)
	}
	return res, nil
}

// ManifestFromURL implements manifest.FileSource#ManifestFromURL
func (p *GHC) ManifestFromURL(url string) (string, error) {
	r, err := p.getResolvedResourceInfo(context.TODO(), url)
	if err != nil {
		return "", err
	}
	content, err := p.Read(context.TODO(), r.String())
	return string(content), err
}

// ToAbsLink implements manifest.FileSource#ToAbsLink
func (p *GHC) ToAbsLink(source, link string) (string, error) {
	r, err := p.getResolvedResourceInfo(context.TODO(), source)
	if err != nil {
		return link, err
	}
	if strings.HasPrefix(link, "http") {
		l, err := p.getResolvedResourceInfo(context.TODO(), link)
		if err != nil {
			return link, err
		}
		link = l.String()
	}
	l, err := url.Parse(strings.TrimSuffix(link, "/"))
	if err != nil {
		return link, err
	}
	if l.IsAbs() {
		return link, nil // already absolute
	}
	// build URL based on source path
	u, err := url.Parse("/" + r.Path)
	if err != nil {
		return link, err
	}
	if u, err = u.Parse(l.Path); err != nil {
		return link, err
	}
	// determine the type of the resource: (blob|tree)
	var tp string
	if tp, err = p.determineLinkType(r, u); err != nil {
		return tp, err
	}
	res, err := url.Parse(r.URL.String())
	if err != nil {
		return "", err
	}
	// set path
	res.Path = fmt.Sprintf("/%s/%s/%s/%s%s", r.Owner, r.Repo, tp, r.Ref, u.Path)
	// set query & fragment
	res.ForceQuery = l.ForceQuery
	res.RawQuery = l.RawQuery
	res.Fragment = l.Fragment

	return res.String(), nil
}

//========================= repositoryhosts.RepositoryHost ===================================================

// Name returns host name
func (p *GHC) Name() string {
	return p.hostName
}

// Accept implements the repositoryhosts.RepositoryHost#Accept
func (p *GHC) Accept(uri string) bool {
	r, err := url.Parse(uri)
	if err != nil || r.Scheme != "https" {
		return false
	}
	for _, h := range p.acceptedHosts {
		if h == r.Host {
			return true
		}
	}
	return false
}

// Read implements the repositoryhosts.RepositoryHost#Read
func (p *GHC) Read(ctx context.Context, uri string) ([]byte, error) {
	r, err := p.getResolvedResourceInfo(ctx, uri)
	if err != nil {
		return nil, err
	}
	if r.Type != "blob" && r.Type != "raw" {
		return nil, fmt.Errorf("not a blob/raw url: %s", r.String())
	}
	if local := p.checkForLocalMapping(r); len(local) > 0 {
		return p.readLocalFile(ctx, r, local)
	}
	// read using GitService and file URL -> file SHA mapping
	if SHA, ok := p.getFileSHA(r.String()); ok {
		raw, resp, err := p.git.GetBlobRaw(ctx, r.Owner, r.Repo, SHA)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, repositoryhosts.ErrResourceNotFound(r.String())
			}
			return nil, err
		}
		if resp != nil && resp.StatusCode >= 400 {
			return nil, fmt.Errorf("reading blob %s fails with HTTP status: %d", r.String(), resp.StatusCode)
		}
		return raw, nil
	}
	// read using RepositoriesService.DownloadContents for non-markdown and non-manifest files - 2 manifestadapter calls
	opt := &github.RepositoryContentGetOptions{Ref: r.Ref}
	if !strings.HasSuffix(strings.ToLower(r.Path), ".md") && !strings.HasSuffix(strings.ToLower(r.Path), ".yaml") {
		return p.downloadContent(ctx, opt, r)
	}
	// read using RepositoriesService.GetContents for markdowns and module manifests - 1 manifestadapter call
	fc, _, resp, err := p.repositories.GetContents(ctx, r.Owner, r.Repo, r.Path, opt)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, repositoryhosts.ErrResourceNotFound(r.String())
		}
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			// if file is bigger than 1 MB -> content should be downloaded
			// it makes two additional manifestadapter cals, but it's unlikely to have large manifest.yaml
			return p.downloadContent(ctx, opt, r)
		}
		return nil, err
	}
	if resp != nil && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("reading blob %s fails with HTTP status: %d", r.String(), resp.StatusCode)
	}
	cnt, err := base64.StdEncoding.DecodeString(*fc.Content)
	if err != nil {
		return nil, err
	}
	return cnt, nil
}

// ReadGitInfo implements the repositoryhosts.RepositoryHost#ReadGitInfo
func (p *GHC) ReadGitInfo(ctx context.Context, uri string) ([]byte, error) {
	r, err := p.getResolvedResourceInfo(ctx, uri)
	if err != nil {
		return nil, err
	}
	opts := &github.CommitsListOptions{
		Path: r.Path,
		SHA:  r.Ref,
	}
	var commits []*github.RepositoryCommit
	var resp *github.Response
	if commits, resp, err = p.repositories.ListCommits(ctx, r.Owner, r.Repo, opts); err != nil {
		return nil, err
	}
	if resp != nil && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("list commits for %s fails with HTTP status: %d", r.String(), resp.StatusCode)
	}
	gitInfo := transform(commits)
	if gitInfo == nil {
		return nil, nil
	}
	if len(r.Ref) > 0 {
		gitInfo.SHAAlias = &r.Ref
	}
	if len(r.Path) > 0 {
		gitInfo.Path = &r.Path
	}
	return json.MarshalIndent(gitInfo, "", "  ")
}

// GetRawFormatLink implements the repositoryhosts.RepositoryHost#GetRawFormatLink
func (p *GHC) GetRawFormatLink(absLink string) (string, error) {
	r, err := link.NewResource(absLink)
	if err != nil {
		return "", err
	}
	if !r.URL.IsAbs() {
		return absLink, nil // don't modify relative links
	}
	return r.GetRawURL(), nil
}

// GetClient implements the repositoryhosts.RepositoryHost#GetClient
func (p *GHC) GetClient() httpclient.Client {
	return p.client
}

// GetRateLimit implements the repositoryhosts.RepositoryHost#GetRateLimit
func (p *GHC) GetRateLimit(ctx context.Context) (int, int, time.Time, error) {
	r, _, err := p.rateLimit.RateLimits(ctx)
	if err != nil {
		return -1, -1, time.Now(), err
	}
	return r.Core.Limit, r.Core.Remaining, r.Core.Reset.Time, nil
}

//==============================================================================================================

// checkForLocalMapping returns repository root on file system if local mapping configuration
// for the repository is set in config file or empty string otherwise.
func (p *GHC) checkForLocalMapping(r *link.Resource) string {
	key := strings.ToLower(r.GetRepoURL())
	if localPath, ok := p.localMappings[key]; ok {
		return localPath
	}
	// repo URLs keys in config file may end with '/'
	return p.localMappings[key+"/"]
}

// readLocalFile reads a file from FS
func (p *GHC) readLocalFile(_ context.Context, r *link.Resource, localPath string) ([]byte, error) {
	fn := filepath.Join(localPath, r.Path)
	cnt, err := p.os.ReadFile(fn)
	if err != nil {
		if p.os.IsNotExist(err) {
			return nil, repositoryhosts.ErrResourceNotFound(r.String())
		}
		return nil, fmt.Errorf("reading file %s for uri %s fails: %v", fn, r.String(), err)
	}
	return cnt, nil
}

func (p *GHC) readLocalFileTree(r link.Resource, localPath string) []string {
	dirPath := filepath.Join(localPath, r.Path)
	files := []string{}
	filepath.Walk(dirPath, func(path string, info fs.FileInfo, err error) error {
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, strings.TrimPrefix(strings.TrimPrefix(path, dirPath), "/"))
		}
		return nil
	})
	return files
}

// downloadContent download file content like: github.Client.Repositories#DownloadContents, but with different error handling
func (p *GHC) downloadContent(ctx context.Context, opt *github.RepositoryContentGetOptions, r *link.Resource) ([]byte, error) {
	dir := path.Dir(r.Path)
	filename := path.Base(r.Path)
	dirContents, resp, err := p.getDirContents(ctx, r.Owner, r.Repo, dir, opt)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, repositoryhosts.ErrResourceNotFound(r.String())
		}
		return nil, err
	}
	for _, contents := range dirContents {
		if *contents.Name == filename {
			if contents.SHA == nil || *contents.SHA == "" {
				return nil, fmt.Errorf("no SHA found for %s", r.String())
			}
			cnt, resp, err := p.git.GetBlobRaw(ctx, r.Owner, r.Repo, *contents.SHA)
			if err != nil {
				if resp != nil && resp.StatusCode == http.StatusNotFound {
					return nil, repositoryhosts.ErrResourceNotFound(r.String())
				}
				return nil, err
			}
			if resp != nil && resp.StatusCode >= 400 {
				return nil, fmt.Errorf("content download for %s fails with HTTP status: %d", r.String(), resp.StatusCode)
			}
			return cnt, nil
		}
	}
	// not found
	return nil, repositoryhosts.ErrResourceNotFound(r.String())
}

// wraps github.Client Repositories.GetContents and synchronize the access to avoid 'unexpected EOF' errors when reading directory content
func (p *GHC) getDirContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (dc []*github.RepositoryContent, resp *github.Response, err error) {
	p.muxCnt.Lock()
	defer p.muxCnt.Unlock()
	_, dc, resp, err = p.repositories.GetContents(ctx, owner, repo, path, opts)
	return
}

// determineLinkType returns the type of relative link (blob|tree)
// repositoryhosts.ErrResourceNotFound if target resource doesn't exist
func (p *GHC) determineLinkType(source *link.Resource, rel *url.URL) (string, error) {
	var tp string
	var err error
	gtp := "tree"
	if len(path.Ext(rel.Path)) > 0 {
		gtp = "blob"
	}
	expURI := fmt.Sprintf("%s://%s/%s/%s/%s/%s%s", source.URL.Scheme, source.URL.Host, source.Owner, source.Repo, gtp, source.Ref, rel.Path)
	// local case
	if local := p.checkForLocalMapping(source); len(local) > 0 {
		fn := filepath.Join(local, rel.Path)
		var info os.FileInfo
		info, err = p.os.Lstat(fn)
		if err != nil {
			if p.os.IsNotExist(err) {
				return expURI, repositoryhosts.ErrResourceNotFound(expURI)
			}
			return "", fmt.Errorf("cannot determine resource type for path %s and source %s: %v", rel.Path, source.String(), err)
		}
		tp = "blob"
		if info.IsDir() {
			tp = "tree"
		}
		return tp, nil
	}
	// list remote repo
	key := fmt.Sprintf("%s://%s/%s/%s/blob/%s%s", source.URL.Scheme, source.URL.Host, source.Owner, source.Repo, source.Ref, rel.Path)
	if _, ok := p.getFileSHA(key); ok {
		tp = "blob" // as file SHA is cached, type is blob
	} else {
		opt := &github.RepositoryContentGetOptions{Ref: source.Ref}
		dir := path.Dir(rel.Path)
		name := path.Base(rel.Path)
		var dc []*github.RepositoryContent
		var resp *github.Response
		p.muxSHA.Lock()
		defer p.muxSHA.Unlock()
		dc, resp, err = p.getDirContents(context.Background(), source.Owner, source.Repo, dir, opt)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound { // parent folder doesn't exist
				uri := fmt.Sprintf("%s://%s/%s/%s/tree/%s%s", source.URL.Scheme, source.URL.Host, source.Owner, source.Repo, source.Ref, dir)
				return expURI, repositoryhosts.ErrResourceNotFound(uri)
			}
			return "", fmt.Errorf("cannot determine resource type for path %s and source %s: %v", rel.Path, source.String(), err)
		}
		for _, d := range dc {
			// add files SHA
			if *d.Type == "blob" {
				p.filesCache[*d.HTMLURL] = *d.SHA
			}
			if *d.Name == name {
				if *d.Type == "file" {
					tp = "blob"
				} else {
					tp = "tree"
				}
			}
		}
	}
	if tp == "" { // resource doesn't exist
		return expURI, repositoryhosts.ErrResourceNotFound(expURI)
	}
	return tp, nil
}

// getResourceInfo build ResourceInfo and resolves 'DEFAULT_BRANCH' to repo default branch
func (p *GHC) getResolvedResourceInfo(ctx context.Context, uri string) (*link.Resource, error) {
	r, err := link.NewResource(uri)
	if err != nil {
		return nil, err
	}
	if r.Ref != "DEFAULT_BRANCH" {
		return &r, nil
	}
	defaultBranch, err := p.getDefaultBranch(ctx, r.Owner, r.Repo)
	if err != nil {
		return nil, err
	}
	r.Ref = defaultBranch
	return &r, nil
}

// getDefaultBranch gets the default branch for given repo
func (p *GHC) getDefaultBranch(ctx context.Context, owner string, repository string) (string, error) {
	p.muxDefBr.Lock()
	defer p.muxDefBr.Unlock()
	key := fmt.Sprintf("%s/%s", owner, repository)
	if def, ok := p.defBranches[key]; ok {
		return def, nil
	}
	repo, _, err := p.repositories.Get(ctx, owner, repository)
	if err != nil {
		return "", err
	}
	def := repo.GetDefaultBranch()
	p.defBranches[key] = def
	return def, nil
}

func (p *GHC) getFileSHA(key string) (string, bool) {
	p.muxSHA.RLock()
	defer p.muxSHA.RUnlock()
	val, ok := p.filesCache[key]
	return val, ok
}

// transform builds git.Info from a commits list
func transform(commits []*github.RepositoryCommit) *GitInfo {
	if commits == nil {
		return nil
	}
	gitInfo := &GitInfo{}
	// skip internal commits
	nonInternalCommits := slices.DeleteFunc(commits, isInternalCommit)
	if len(nonInternalCommits) == 0 {
		return nil
	}
	sort.Slice(nonInternalCommits, func(i, j int) bool {
		return nonInternalCommits[i].GetCommit().GetCommitter().GetDate().After(nonInternalCommits[j].GetCommit().GetCommitter().GetDate())
	})
	lastModifiedDate := nonInternalCommits[0].GetCommit().GetCommitter().GetDate().Format(DateFormat)
	gitInfo.LastModifiedDate = &lastModifiedDate

	webURL := nonInternalCommits[0].GetHTMLURL()
	gitInfo.WebURL = github.String(strings.Split(webURL, "/commit/")[0])

	gitInfo.PublishDate = github.String(nonInternalCommits[len(nonInternalCommits)-1].GetCommit().GetCommitter().GetDate().Format(DateFormat))

	if gitInfo.Author = getCommitAuthor(nonInternalCommits[len(nonInternalCommits)-1]); gitInfo.Author == nil {
		klog.Warningf("cannot get commit author")
	}
	if len(nonInternalCommits) < 2 {
		return gitInfo
	}
	gitInfo.Contributors = []*github.User{}
	var registered []string
	for _, commit := range nonInternalCommits {
		var contributor *github.User
		if contributor = getCommitAuthor(commit); contributor == nil {
			continue
		}
		if contributor.GetType() == "User" && contributor.GetEmail() != gitInfo.Author.GetEmail() && slices.Index(registered, contributor.GetEmail()) < 0 {
			gitInfo.Contributors = append(gitInfo.Contributors, contributor)
			registered = append(registered, contributor.GetEmail())
		}
	}
	return gitInfo
}

func isInternalCommit(commit *github.RepositoryCommit) bool {
	message := commit.GetCommit().GetMessage()
	email := commit.GetCommitter().GetEmail()
	return strings.HasPrefix(message, "[int]") ||
		strings.Contains(message, "[skip ci]") ||
		strings.HasPrefix(email, "gardener.ci") ||
		strings.HasPrefix(email, "gardener.opensource")
}

func getCommitAuthor(commit *github.RepositoryCommit) *github.User {
	getCommitAuthor := commit.GetCommit().GetAuthor()
	getCommitCommiter := commit.GetCommit().GetCommitter()
	contributor := commit.GetAuthor()
	if contributor != nil && getCommitAuthor != nil {
		contributor.Name = getCommitAuthor.Name
		contributor.Email = getCommitAuthor.Email
		return contributor
	}
	if getCommitAuthor != nil {
		return &github.User{Name: getCommitAuthor.Name, Email: getCommitAuthor.Email}
	}
	if getCommitCommiter != nil {
		return &github.User{Name: getCommitCommiter.Name, Email: getCommitCommiter.Email}
	}
	return nil
}
