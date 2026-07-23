package stack

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KCaverly/caretaker/internal/repo"
)

type SetupOptions struct {
	Dir              string
	EnableAutoDelete bool
}

type RepositorySettings struct {
	Repository          string `json:"repository"`
	DeleteBranchOnMerge bool   `json:"delete_branch_on_merge"`
}

type repositoryResponse struct {
	FullName            string `json:"full_name"`
	DeleteBranchOnMerge bool   `json:"delete_branch_on_merge"`
}

func repoSettingsArgs(repository string) []string {
	return []string{"api", "repos/" + repository}
}

func enableAutoDeleteArgs(repository string) []string {
	return []string{"api", "--method", "PATCH", "repos/" + repository, "-F", "delete_branch_on_merge=true"}
}

func repositoryName(dir string) (string, error) {
	out, err := runGH(dir, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" || !strings.Contains(name, "/") {
		return "", fmt.Errorf("could not resolve GitHub repository name")
	}
	return name, nil
}

func readRepositorySettings(dir, repository string) (RepositorySettings, error) {
	out, err := runGH(dir, repoSettingsArgs(repository)...)
	if err != nil {
		return RepositorySettings{}, err
	}
	var response repositoryResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return RepositorySettings{}, fmt.Errorf("decoding GitHub repository settings: %w", err)
	}
	if response.FullName != "" && response.FullName != repository {
		return RepositorySettings{}, fmt.Errorf("GitHub returned settings for %s, expected %s", response.FullName, repository)
	}
	return RepositorySettings{Repository: repository, DeleteBranchOnMerge: response.DeleteBranchOnMerge}, nil
}

// Setup inspects repository-level settings used by stack cleanup. Mutation is
// explicit and followed by an independent readback so a permission failure or
// ignored update cannot be reported as success.
func Setup(o SetupOptions) (RepositorySettings, error) {
	if _, err := repo.Git(o.Dir, "rev-parse", "--show-toplevel"); err != nil {
		return RepositorySettings{}, fmt.Errorf("not inside a git repository: %w", err)
	}
	if err := requireGH(); err != nil {
		return RepositorySettings{}, err
	}
	repository, err := repositoryName(o.Dir)
	if err != nil {
		return RepositorySettings{}, err
	}
	if o.EnableAutoDelete {
		if _, err := runGH(o.Dir, enableAutoDeleteArgs(repository)...); err != nil {
			return RepositorySettings{}, fmt.Errorf("enabling automatic branch deletion: %w", err)
		}
	}
	settings, err := readRepositorySettings(o.Dir, repository)
	if err != nil {
		return settings, err
	}
	if o.EnableAutoDelete && !settings.DeleteBranchOnMerge {
		return settings, fmt.Errorf("GitHub did not retain delete_branch_on_merge=true for %s", repository)
	}
	return settings, nil
}
