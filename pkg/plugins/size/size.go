/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package size contains a Prow plugin which counts the number of lines changed
// in a pull request, buckets this number into a few size classes (S, L, XL, etc),
// and finally labels the pull request with this size.
package size

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/genfiles"
	"sigs.k8s.io/prow/pkg/gitattributes"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/pluginhelp"
	"sigs.k8s.io/prow/pkg/plugins"
)

// The sizes are configurable in the `plugins.yaml` config file; the line constants
// in here represent default values used as fallback if none are provided.
const pluginName = "size"

var defaultSizes = plugins.Size{
	S:   10,
	M:   30,
	L:   100,
	Xl:  500,
	Xxl: 1000,
}

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, _ []config.OrgRepo) (*pluginhelp.PluginHelp, error) {
	sizes := sizesOrDefault(config.Size)
	yamlSnippet, err := plugins.CommentMap.GenYaml(&plugins.Configuration{
		Size: plugins.Size{
			S:   10,
			M:   30,
			L:   100,
			Xl:  500,
			Xxl: 1000,
		},
	})
	if err != nil {
		logrus.WithError(err).Warnf("cannot generate comments for %s plugin", pluginName)
	}
	return &pluginhelp.PluginHelp{
			Description: "The size plugin manages the 'size/*' labels, maintaining the appropriate label on each pull request as it is updated. Generated files identified by the config file '.generated_files' at the repo root are ignored. Labels are applied based on the total number of lines of changes (additions and deletions).",
			Config: map[string]string{
				"": fmt.Sprintf(`The plugin has the following thresholds:<ul>
<li>size/XS:  0-%d</li>
<li>size/S:   %d-%d</li>
<li>size/M:   %d-%d</li>
<li>size/L:   %d-%d</li>
<li>size/XL:  %d-%d</li>
<li>size/XXL: %d+</li>
</ul>`, sizes.S-1, sizes.S, sizes.M-1, sizes.M, sizes.L-1, sizes.L, sizes.Xl-1, sizes.Xl, sizes.Xxl-1, sizes.Xxl),
			},
			Snippet: yamlSnippet,
		},
		nil
}

func handlePullRequest(pc plugins.Agent, pe github.PullRequestEvent) error {
	return handlePR(pc.GitHubClient, sizesOrDefault(pc.PluginConfig.Size), pc.Logger, pe)
}

// Strict subset of github.Client methods.
type githubClient interface {
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetFile(org, repo, filepath, commit string) ([]byte, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

func handlePR(gc githubClient, sizes plugins.Size, le *logrus.Entry, pe github.PullRequestEvent) error {
	if !isPRChanged(pe) {
		return nil
	}

	var (
		owner = pe.PullRequest.Base.Repo.Owner.Login
		repo  = pe.PullRequest.Base.Repo.Name
		num   = pe.PullRequest.Number
		sha   = pe.PullRequest.Base.SHA
	)

	gf, err := genfiles.NewGroup(gc, owner, repo, sha)
	if err != nil {
		switch err.(type) {
		case *genfiles.ParseError:
			// Continue on parse errors, but warn that something is wrong.
			le.Warnf("error while parsing .generated_files: %v", err)
		default:
			return err
		}
	}

	ga, err := gitattributes.NewGroup(func() ([]byte, error) { return gc.GetFile(owner, repo, ".gitattributes", sha) })
	if err != nil {
		return err
	}

	changes, err := gc.GetPullRequestChanges(owner, repo, num)
	if err != nil {
		return fmt.Errorf("can not get PR changes for size plugin: %w", err)
	}

	var count int
	for _, change := range changes {
		// Skip generated and linguist-generated files.
		if gf.Match(change.Filename) || ga.IsLinguistGenerated(change.Filename) {
			continue
		}

		count += change.Additions + change.Deletions
	}

	labels, err := gc.GetIssueLabels(owner, repo, num)
	if err != nil {
		le.Warnf("while retrieving labels, error: %v", err)
	}

	newLabel := bucket(count, sizes).label()
	var hasLabel bool

	for _, label := range labels {
		if label.Name == newLabel {
			hasLabel = true
			continue
		}

		if strings.HasPrefix(label.Name, labelPrefix) {
			if err := gc.RemoveLabel(owner, repo, num, label.Name); err != nil {
				le.Warnf("error while removing label %q: %v", label.Name, err)
			}
		}
	}

	if hasLabel {
		return nil
	}

	if err := gc.AddLabel(owner, repo, num, newLabel); err != nil {
		return fmt.Errorf("error adding label to %s/%s PR #%d: %w", owner, repo, num, err)
	}

	return nil
}

// One of a set of discrete buckets.
type size int

const (
	sizeXS size = iota
	sizeS
	sizeM
	sizeL
	sizeXL
	sizeXXL
)

const (
	labelPrefix = "size/"

	labelXS      = "size/XS"
	labelS       = "size/S"
	labelM       = "size/M"
	labelL       = "size/L"
	labelXL      = "size/XL"
	labelXXL     = "size/XXL"
	labelUnknown = "size/?"
)

func (s size) label() string {
	switch s {
	case sizeXS:
		return labelXS
	case sizeS:
		return labelS
	case sizeM:
		return labelM
	case sizeL:
		return labelL
	case sizeXL:
		return labelXL
	case sizeXXL:
		return labelXXL
	}

	return labelUnknown
}

func bucket(lineCount int, sizes plugins.Size) size {
	if lineCount < sizes.S {
		return sizeXS
	} else if lineCount < sizes.M {
		return sizeS
	} else if lineCount < sizes.L {
		return sizeM
	} else if lineCount < sizes.Xl {
		return sizeL
	} else if lineCount < sizes.Xxl {
		return sizeXL
	}

	return sizeXXL
}

// These are the only actions indicating the code diffs may have changed.
func isPRChanged(pe github.PullRequestEvent) bool {
	switch pe.Action {
	case github.PullRequestActionOpened:
		return true
	case github.PullRequestActionReopened:
		return true
	case github.PullRequestActionSynchronize:
		return true
	case github.PullRequestActionEdited:
		return true
	default:
		return false
	}
}

func defaultIfZero(value, defaultValue int) int {
	if value == 0 {
		return defaultValue
	}
	return value
}

func sizesOrDefault(sizes plugins.Size) plugins.Size {
	sizes.S = defaultIfZero(sizes.S, defaultSizes.S)
	sizes.M = defaultIfZero(sizes.M, defaultSizes.M)
	sizes.L = defaultIfZero(sizes.L, defaultSizes.L)
	sizes.Xl = defaultIfZero(sizes.Xl, defaultSizes.Xl)
	sizes.Xxl = defaultIfZero(sizes.Xxl, defaultSizes.Xxl)
	return sizes
}
