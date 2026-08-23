package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v90/github"
	"github.com/google/uuid"
	"github.com/nxrmqlly/jittrippin/internal/daemon"
	"github.com/nxrmqlly/jittrippin/internal/store"
	"github.com/nxrmqlly/jittrippin/pkg/engine"
	"golang.org/x/oauth2"
)

var ErrInstallNotFound = errors.New("github: app is not installed for repository owner")

// Integration is the Github bot
type Integration struct {
	appTS oauth2.TokenSource
	st    *store.Store
	mgr   *daemon.Manager
	srcs  *installTokenSourceCache
	pub   string
	slug  string
}

type Config struct {
	AppID      string
	PrivateKey string
	PublicURL  string
	AppSlug    string
}

func New(cfg Config, st *store.Store, mgr *daemon.Manager) (*Integration, error) {
	appTS, err := loadAppTokenSrc(cfg.AppID, cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &Integration{
		appTS: appTS,
		st:    st,
		mgr:   mgr,
		srcs:  newInstallTokenSourceCache(),
		pub:   cfg.PublicURL,
		slug:  cfg.AppSlug,
	}, nil
}

func (i *Integration) Name() string {
	return "github"
}

func (i *Integration) installForOwner(ctx context.Context, cu *Client, owner string) (int64, error) {
	installs, err := cu.ListUserInstallations(ctx)
	if err != nil {
		return 0, err
	}
	for _, inst := range installs {
		if inst.AccountLogin == owner {
			return inst.ID, nil
		}
	}
	return 0, ErrInstallNotFound
}

func (i *Integration) SubmitPush(ctx context.Context, tracked store.TrackedRepository, ref, sha string, installID int64) ([]*daemon.Run, error) {
	ts := i.srcs.Get(installID, i.appTS)

	c, err := forInstallation(ts)
	if err != nil {
		return nil, err
	}

	tok, err := ts.Token()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(tracked.FullName, "/", 2)
	if len(parts) != 2 {
		return nil, errors.New("github: invalid full_name " + tracked.FullName)
	}
	owner, name := parts[0], parts[1]
	pipelines, err := LoadPipelinesAtSHA(ctx, c, owner, name, sha)
	if err != nil {
		return nil, err
	}

	var runs []*daemon.Run
	for _, p := range pipelines {
		if !matchesPush(p, ref) {
			continue
		}
		p.Checkout.URL = "https://github.com/" + tracked.FullName + ".git"
		p.Checkout.Ref = sha
		p.Checkout.Username = "x-access-token"
		p.Checkout.Password = tok.AccessToken

		run, err := i.mgr.Submit(tracked.OwnerID, p)
		if err != nil {
			return runs, err
		}
		runs = append(runs, run)

		statusCtx := "jittrippin/" + p.Name
		target := i.pub + "/api/v1/runs/" + run.ID()
		if err := c.CreateCommitStatus(ctx, CreateCommitStatusConfig{
			Owner:     owner,
			Name:      name,
			SHA:       sha,
			State:     "pending",
			Context:   statusCtx,
			Desc:      "jittrippin: run started",
			TargetURL: target,
		}); err != nil {
			slog.Error("github: SubmitPush: pending status failed", "err", err)
		}
		go i.reportCompletion(ctx, c, owner, name, sha, p.Name, run, target)
	}
	return runs, nil
}

type ReleaseInfo struct {
	Action    string
	Tag       string
	ReleaseID int64
}

const defaultReleaseOn = "published"

func (i *Integration) SubmitRelease(ctx context.Context, tracked store.TrackedRepository, installID int64, info ReleaseInfo) ([]*daemon.Run, error) {
	slog.Info("github: SubmitRelease: starting", "repo", tracked.FullName, "tag", info.Tag, "action", info.Action, "release_id", info.ReleaseID, "install_id", installID)

	ts := i.srcs.Get(installID, i.appTS)
	c, err := forInstallation(ts)
	if err != nil {
		slog.Error("github: SubmitRelease: forInstallation failed", "err", err)
		return nil, err
	}
	tok, err := ts.Token()
	if err != nil {
		slog.Error("github: SubmitRelease: token fetch failed", "err", err)
		return nil, err
	}
	parts := strings.SplitN(tracked.FullName, "/", 2)
	if len(parts) != 2 {
		return nil, errors.New("github: invalid full_name " + tracked.FullName)
	}
	owner, name := parts[0], parts[1]

	sha, err := c.ResolveTagSHA(ctx, owner, name, info.Tag)
	if err != nil {
		slog.Error("github: SubmitRelease: ResolveTagSHA failed", "tag", info.Tag, "err", err)
		return nil, err
	}
	slog.Info("github: SubmitRelease: resolved tag", "tag", info.Tag, "sha", sha)

	pipelines, err := LoadPipelinesAtSHA(ctx, c, owner, name, sha)
	if err != nil {
		slog.Error("github: SubmitRelease: LoadPipelinesAtSHA failed", "sha", sha, "err", err)
		return nil, err
	}
	slog.Info("github: SubmitRelease: loaded pipelines", "count", len(pipelines), "sha", sha)

	var runs []*daemon.Run
	for _, p := range pipelines {
		rel := releaseConfig(p)
		if rel == nil {
			slog.Debug("github: SubmitRelease: skipping pipeline (no release config)", "pipeline", p.Name)
			continue
		}
		on := rel.On
		if on == "" {
			on = defaultReleaseOn
		}
		if on != info.Action {
			slog.Debug("github: SubmitRelease: skipping pipeline (action mismatch)", "pipeline", p.Name, "pipeline_on", on, "event_action", info.Action)
			continue
		}
		slog.Info("github: SubmitRelease: running pipeline", "pipeline", p.Name, "on", on)
		p.Checkout.URL = "https://github.com/" + tracked.FullName + ".git"
		p.Checkout.Ref = sha
		p.Checkout.Username = "x-access-token"
		p.Checkout.Password = tok.AccessToken

		run, err := i.mgr.Submit(tracked.OwnerID, p)
		if err != nil {
			slog.Error("github: SubmitRelease: mgr.Submit failed", "pipeline", p.Name, "err", err)
			return runs, err
		}
		runs = append(runs, run)

		statusCtx := "jittrippin/" + p.Name
		target := i.pub + "/api/v1/runs/" + run.ID()
		slog.Info("github: SubmitRelease: run submitted", "pipeline", p.Name, "run_id", run.ID(), "target", target)
		if err := c.CreateCommitStatus(ctx, CreateCommitStatusConfig{
			Owner: owner, Name: name, SHA: sha, State: "pending",
			Context: statusCtx, Desc: "jittrippin: release run started", TargetURL: target,
		}); err != nil {
			slog.Error("github: SubmitRelease: pending status failed", "err", err)
		}
		go i.reportRelease(ctx, c, owner, name, sha, p, run, target, tracked.OwnerID, info)
	}
	slog.Info("github: SubmitRelease: done", "runs", len(runs))
	return runs, nil
}

func (i *Integration) reportRelease(ctx context.Context, c *Client, owner, name, sha string, p *engine.Pipeline, run *daemon.Run, target, ownerID string, info ReleaseInfo) {
	slog.Info("github: reportRelease: run completed, waiting...", "pipeline", p.Name, "run_id", run.ID())
	run.Wait()

	if run.Status() == daemon.StatusFailed {
		slog.Error("github: reportRelease: run failed", "pipeline", p.Name, "run_id", run.ID())
		i.setReleaseStatus(ctx, c, owner, name, sha, p.Name, "failure", "jittrippin: "+p.Name+" [failed]", target)
		return
	}

	rel := releaseConfig(p)
	if rel == nil {
		return
	}
	slog.Info("github: reportRelease: uploading artifacts", "pipeline", p.Name, "count", len(rel.Artifacts))
	uploaded := 0
	for _, ra := range rel.Artifacts {
		assetName := ra.As
		if assetName == "" {
			assetName = ra.Name
		}
		if !strings.HasSuffix(assetName, ".tar") {
			assetName += ".tar"
		}

		slog.Debug("github: reportRelease: loading artifact", "job", ra.Job, "artifact", ra.Name, "asset_name", assetName)
		rc, err := i.mgr.GetArtifact(ctx, daemon.GetArtifactConfig{
			RunID: run.ID(), UserID: ownerID, JobName: ra.Job, ArtifactName: ra.Name,
		})
		if err != nil {
			slog.Error("github: reportRelease: artifact load failed", "job", ra.Job, "artifact", ra.Name, "err", err)
			i.setReleaseStatus(ctx, c, owner, name, sha, p.Name, "failure", "jittrippin: "+p.Name+" [artifact load failed]", target)
			return
		}
		if err := i.uploadReleaseAsset(ctx, c, owner, name, info.ReleaseID, assetName, rc); err != nil {
			rc.Close()
			slog.Error("github: reportRelease: asset upload failed", "asset_name", assetName, "err", err)
			i.setReleaseStatus(ctx, c, owner, name, sha, p.Name, "failure", "jittrippin: "+p.Name+" [asset upload failed]", target)
			return
		}
		rc.Close()
		slog.Info("github: reportRelease: asset uploaded", "asset_name", assetName)
		uploaded++
	}
	slog.Info("github: reportRelease: all assets uploaded", "pipeline", p.Name, "uploaded", uploaded)
	i.setReleaseStatus(ctx, c, owner, name, sha, p.Name, "success",
		fmt.Sprintf("jittrippin: %s [passed] (%d assets)", p.Name, uploaded), target)
}

func (i *Integration) uploadReleaseAsset(ctx context.Context, c *Client, owner, name string, releaseID int64, assetName string, rc io.ReadCloser) error {
	slog.Debug("github: uploadReleaseAsset: buffering", "asset_name", assetName, "release_id", releaseID)
	f, err := os.CreateTemp("", "jt-release-*")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())

	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, _ := f.Stat()
	slog.Debug("github: uploadReleaseAsset: buffered", "asset_name", assetName, "size", info.Size())

	assets, err := c.ListReleaseAssets(ctx, owner, name, releaseID)
	if err != nil {
		slog.Error("github: uploadReleaseAsset: ListReleaseAssets failed", "err", err)
		return err
	}
	for _, a := range assets {
		if a.GetName() == assetName {
			slog.Debug("github: uploadReleaseAsset: deleting existing asset", "asset_name", assetName, "asset_id", a.GetID())
			if err := c.DeleteReleaseAsset(ctx, owner, name, a.GetID()); err != nil {
				return err
			}
			break
		}
	}

	slog.Debug("github: uploadReleaseAsset: uploading", "asset_name", assetName, "size", info.Size())
	_, err = c.UploadReleaseAsset(ctx, owner, name, releaseID, assetName, f)
	if err != nil {
		slog.Error("github: uploadReleaseAsset: UploadReleaseAsset failed", "asset_name", assetName, "err", err)
	} else {
		slog.Info("github: uploadReleaseAsset: uploaded", "asset_name", assetName, "size", info.Size())
	}
	return err
}

func (i *Integration) setReleaseStatus(ctx context.Context, c *Client, owner, name, sha, pipelineName, state, descr, target string) {
	slog.Info("github: setReleaseStatus", "pipeline", pipelineName, "state", state, "descr", descr)
	if err := c.CreateCommitStatus(ctx, CreateCommitStatusConfig{
		Owner: owner, Name: name, SHA: sha, State: state,
		Context: "jittrippin/" + pipelineName, Desc: descr, TargetURL: target,
	}); err != nil {
		slog.Error("github: setReleaseStatus: failed", "pipeline", pipelineName, "err", err)
	}
}

func (i *Integration) reportCompletion(ctx context.Context, c *Client, owner, name, sha, pipelineName string, run *daemon.Run, target string) {
	run.Wait()
	state := "success"
	descr := "jittrippin: " + pipelineName + " [passed]"
	if run.Status() == daemon.StatusFailed {
		state = "failure"
		descr = "jittrippin: " + pipelineName + " [failed]"
	}
	if err := c.CreateCommitStatus(ctx, CreateCommitStatusConfig{
		Owner:     owner,
		Name:      name,
		SHA:       sha,
		State:     state,
		Context:   "jittrippin/" + pipelineName,
		Desc:      descr,
		TargetURL: target,
	}); err != nil {
		slog.Error("github: reportCompletion: status failed", "pipeline", pipelineName, "err", err)
	}

}

func (i *Integration) TrackRepository(ctx context.Context, userToken, ownerID, fullName, branch string) error {
	cu, err := forUser(userToken)
	if err != nil {
		return err
	}

	owner, name, _ := strings.Cut(fullName, "/")
	installID, err := i.installForOwner(ctx, cu, owner)
	if err != nil {
		return err
	}

	ts := i.srcs.Get(installID, i.appTS)
	c, err := forInstallation(ts)
	if err != nil {
		return err
	}

	repo, err := c.FetchRepository(ctx, owner, name)
	if err != nil {
		return err
	}

	return i.mgr.TrackRepository(daemon.TrackRepositoryConfig{
		OwnerID:              ownerID,
		Provider:             "github",
		ProviderInstance:     "github.com",
		ProviderRepositoryID: repo.ID,
		FullName:             repo.FullName,
		Branch:               branch,
	})
}

func (i *Integration) RecordInstall(ctx context.Context, installID int64, setupAction string) error {
	httpC := oauth2.NewClient(ctx, i.appTS)
	c, err := gh.NewClient(gh.WithHTTPClient(httpC))
	if err != nil {
		return err
	}
	inst, _, err := c.Apps.GetInstallation(ctx, installID)
	if err != nil {
		return err
	}
	rec := &store.Installation{
		ID:               strconv.FormatInt(installID, 10),
		Provider:         "github",
		ProviderInstance: "github.com",
		SetupAction:      setupAction,
	}
	if a := inst.GetAccount(); a != nil {
		rec.AccountLogin = a.GetLogin()
		rec.AccountType = a.GetType()
	}
	return i.st.UpsertInstallation(ctx, rec)
}

type InstallStatusResult struct {
	Installed  bool
	Accounts   []Installation
	InstallURL string
}

func (i *Integration) InstallStatus(ctx context.Context, userToken string) (InstallStatusResult, error) {
	cu, err := forUser(userToken)
	if err != nil {
		return InstallStatusResult{}, err
	}
	installs, err := cu.ListUserInstallations(ctx)
	if err != nil {
		return InstallStatusResult{}, err
	}
	return InstallStatusResult{
		Installed:  len(installs) > 0,
		Accounts:   installs,
		InstallURL: "https://github.com/apps/" + i.slug + "/installations/new",
	}, nil
}

func (i *Integration) ListBranches(ctx context.Context, userToken, fullName string) ([]string, error) {
	owner, name, _ := strings.Cut(fullName, "/")

	cu, err := forUser(userToken)
	if err != nil {
		return nil, err
	}
	installID, err := i.installForOwner(ctx, cu, owner)
	if err != nil {
		return nil, err
	}
	ts := i.srcs.Get(installID, i.appTS)
	c, err := forInstallation(ts)
	if err != nil {
		return nil, err
	}
	return c.ListBranches(ctx, owner, name)
}

func (i *Integration) ListInstallableRepos(ctx context.Context, userToken string) ([]Repository, error) {
	cu, err := forUser(userToken)
	if err != nil {
		return nil, err
	}
	installs, err := cu.ListUserInstallations(ctx)
	if err != nil {
		return nil, err
	}
	var out []Repository
	for _, inst := range installs {
		ts := i.srcs.Get(inst.ID, i.appTS)
		c, err := forInstallation(ts)
		if err != nil {
			continue
		}
		repos, err := c.ListInstallationRepos(ctx)
		if err != nil {
			continue
		}
		out = append(out, repos...)
	}
	return out, nil
}

func (i *Integration) LinkUserIntegration(ctx context.Context, userID string, userToken string) ([]Installation, error) {
	cu, err := forUser(userToken)
	if err != nil {
		return nil, err
	}
	installs, err := cu.ListUserInstallations(ctx)
	if err != nil {
		return nil, err
	}
	ui := &store.UserIntegration{
		ID:               uuid.NewString(), // or helpers like other store calls
		UserID:           userID,
		Provider:         "github",
		ProviderInstance: "github.com",
		Status:           "active",
	}
	for _, inst := range installs {
		ui.Installations = append(ui.Installations, store.IntegrationInstallation{
			InstallationID: strconv.FormatInt(inst.ID, 10),
			AccountLogin:   inst.AccountLogin,
			AccountType:    inst.AccountType,
		})
	}
	if err := i.st.UpsertIntegration(ctx, ui); err != nil {
		return nil, err
	}
	return installs, nil
}

func (i *Integration) ListUserIntegrations(ctx context.Context, userID string) ([]store.UserIntegration, error) {
	return i.st.ListUserIntegrations(ctx, userID)
}

func (i *Integration) UnlinkUserIntegration(ctx context.Context, userID, provider string) error {
	if err := i.st.DeleteIntegration(ctx, userID, provider); err != nil {
		return err
	}
	// cascade delete repos
	return i.st.DeleteTrackedRepositoriesByProvider(ctx, userID, provider)
}

func pushConfig(p *engine.Pipeline) *engine.PushConfig {
	if p == nil || p.GitHub == nil {
		return nil
	}
	return p.GitHub.Push
}

func releaseConfig(p *engine.Pipeline) *engine.ReleaseConfig {
	if p == nil || p.GitHub == nil {
		return nil
	}
	return p.GitHub.Release
}

func matchesPush(p *engine.Pipeline, ref string) bool {
	if p.GitHub == nil {
		return true // legacy: no github block, keep running on push
	}
	pc := p.GitHub.Push
	if pc == nil {
		return false // declared github but no push trigger
	}

	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		if len(pc.Branches) == 0 {
			return len(pc.Tags) == 0
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		for _, g := range pc.Branches {
			if ok, _ := path.Match(g, name); ok {
				return true
			}
		}
		return false
	case strings.HasPrefix(ref, "refs/tags/"):
		if len(pc.Tags) == 0 {
			return len(pc.Branches) == 0
		}
		name := strings.TrimPrefix(ref, "refs/tags/")
		for _, g := range pc.Tags {
			if ok, _ := path.Match(g, name); ok {
				return true
			}
		}
		return false
	default:
		return len(pc.Branches) == 0 && len(pc.Tags) == 0
	}
}
