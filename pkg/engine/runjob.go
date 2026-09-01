package engine

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/nxrmqlly/jittrippin/pkg/artifact"
	"github.com/nxrmqlly/jittrippin/pkg/checkout"
	"github.com/nxrmqlly/jittrippin/pkg/runner"
)

type ExecuteJobConfig struct {
	runner        runner.Runner
	artifactStore artifact.Store
	checkouter    checkout.Checkouter
	runtime       *PipelineRuntime
	job           *Job
	stdout        io.Writer
	stderr        io.Writer
}

func (p *Pipeline) LookupArtifact(jobName, artifactName string) (artifact.Artifact, error) {
	for _, job := range p.Jobs {
		if jobName != job.Name {
			continue
		}

		for _, a := range job.Artifacts {
			if a.Name == artifactName {
				return a, nil
			}
		}
		return artifact.Artifact{}, fmt.Errorf("job '%s' does not produce artifact '%s'", jobName, artifactName)
	}
	return artifact.Artifact{}, fmt.Errorf("job '%s' does not exist", jobName)
}

// ExecuteJ
func ExecuteJob(ctx context.Context, cfg ExecuteJobConfig) error {
	exec, err := cfg.runner.Create(ctx, runner.ExecutionCreateConfig{
		Image: cfg.job.Image,
		Env:   cfg.job.Env,
	})
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		exec.Remove(context.Background())
	}()

	defer func() {
		if err := exec.Remove(ctx); err != nil {
			log.Printf("cleanup failed: %v", err)
		}
	}()

	co := cfg.runtime.pipeline.Checkout
	if co.URL != "" {
		r, err := cfg.checkouter.Checkout(ctx, co)
		if err != nil {
			return err
		}

		if err := exec.CopyIn(ctx, r, cfg.runner.WorkDir()); err != nil {
			r.Close()
			return err
		}
		r.Close()

	}

	for _, dep := range cfg.job.DependsOn {
		for _, req := range dep.Requires {
			a, err := cfg.runtime.pipeline.LookupArtifact(dep.Job, req)
			if err != nil {
				return err
			}

			r, err := cfg.artifactStore.Load(ctx, artifact.Ref{
				RunID: cfg.runtime.ID(),
				Path:  artifact.ArtifactPath(dep.Job, req),
			})
			if err != nil {
				return err
			}

			if err := exec.CopyIn(ctx, r, a.Path); err != nil {
				r.Close()
				return err
			}

			r.Close()
		}
	}

	for _, step := range cfg.job.Steps {
		_, err := exec.Exec(ctx, runner.ExecConfig{
			Cmd:    step.Cmd,
			Stdout: cfg.stdout,
			Stderr: cfg.stderr,
		})
		if err != nil {
			return err
		}
	}

	for _, a := range cfg.job.Artifacts {
		r, err := exec.CopyOut(ctx, a.Path)
		if err != nil {
			return err
		}

		w, err := cfg.artifactStore.Create(
			ctx,
			artifact.Ref{
				RunID: cfg.runtime.ID(),
				Path:  artifact.ArtifactPath(cfg.job.Name, a.Name),
			},
		)
		if err != nil {
			return err
		}
		defer w.Close()

		if _, err := io.Copy(w, r); err != nil {
			return err
		}

		if err := r.Close(); err != nil {
			return err
		}
	}

	return nil
}
