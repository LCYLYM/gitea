// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package private includes all internal routes. The package name internal is ideal but Golang is not allowed, so we use private as package name instead.
package private

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/private"
	"code.gitea.io/gitea/modules/repofiles"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	pull_service "code.gitea.io/gitea/services/pull"

	"gitea.com/macaron/macaron"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/gobwas/glob"
)

func verifyCommits(oldCommitID, newCommitID string, repo *git.Repository, env []string) error {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	err = git.NewCommand("rev-list", oldCommitID+"..."+newCommitID).
		RunInDirTimeoutEnvFullPipelineFunc(env, -1, repo.Path,
			stdoutWriter, nil, nil,
			func(ctx context.Context, cancel context.CancelFunc) error {
				_ = stdoutWriter.Close()
				err := readAndVerifyCommitsFromShaReader(stdoutReader, repo, env)
				if err != nil {
					log.Error("%v", err)
					cancel()
				}
				_ = stdoutReader.Close()
				return err
			})
	if err != nil && !isErrUnverifiedCommit(err) {
		log.Error("Unable to check commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}
	return err
}

func checkFileProtection(oldCommitID, newCommitID string, patterns []glob.Glob, repo *git.Repository, env []string) error {

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create os.Pipe for %s", repo.Path)
		return err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()

	err = git.NewCommand("diff", "--name-only", oldCommitID+"..."+newCommitID).
		RunInDirTimeoutEnvFullPipelineFunc(env, -1, repo.Path,
			stdoutWriter, nil, nil,
			func(ctx context.Context, cancel context.CancelFunc) error {
				_ = stdoutWriter.Close()

				scanner := bufio.NewScanner(stdoutReader)
				for scanner.Scan() {
					path := strings.TrimSpace(scanner.Text())
					if len(path) == 0 {
						continue
					}
					lpath := strings.ToLower(path)
					for _, pat := range patterns {
						if pat.Match(lpath) {
							cancel()
							return models.ErrFilePathProtected{
								Path: path,
							}
						}
					}
				}
				if err := scanner.Err(); err != nil {
					return err
				}
				_ = stdoutReader.Close()
				return err
			})
	if err != nil && !models.IsErrFilePathProtected(err) {
		log.Error("Unable to check file protection for commits from %s to %s in %s: %v", oldCommitID, newCommitID, repo.Path, err)
	}
	return err
}

func readAndVerifyCommitsFromShaReader(input io.ReadCloser, repo *git.Repository, env []string) error {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		err := readAndVerifyCommit(line, repo, env)
		if err != nil {
			log.Error("%v", err)
			return err
		}
	}
	return scanner.Err()
}

func readAndVerifyCommit(sha string, repo *git.Repository, env []string) error {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to create pipe for %s: %v", repo.Path, err)
		return err
	}
	defer func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}()
	hash := plumbing.NewHash(sha)

	return git.NewCommand("cat-file", "commit", sha).
		RunInDirTimeoutEnvFullPipelineFunc(env, -1, repo.Path,
			stdoutWriter, nil, nil,
			func(ctx context.Context, cancel context.CancelFunc) error {
				_ = stdoutWriter.Close()
				commit, err := git.CommitFromReader(repo, hash, stdoutReader)
				if err != nil {
					return err
				}
				verification := models.ParseCommitWithSignature(commit)
				if !verification.Verified {
					cancel()
					return &errUnverifiedCommit{
						commit.ID.String(),
					}
				}
				return nil
			})
}

type errUnverifiedCommit struct {
	sha string
}

func (e *errUnverifiedCommit) Error() string {
	return fmt.Sprintf("Unverified commit: %s", e.sha)
}

func isErrUnverifiedCommit(err error) bool {
	_, ok := err.(*errUnverifiedCommit)
	return ok
}

// HookPreReceive checks whether a individual commit is acceptable
func HookPreReceive(ctx *macaron.Context, opts private.HookOptions) {
	ownerName := ctx.Params(":owner")
	repoName := ctx.Params(":repo")
	repo, err := models.GetRepositoryByOwnerAndName(ownerName, repoName)
	if err != nil {
		log.Error("Unable to get repository: %s/%s Error: %v", ownerName, repoName, err)
		ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"err": err.Error(),
		})
		return
	}
	repo.OwnerName = ownerName
	gitRepo, err := git.OpenRepository(repo.RepoPath())
	if err != nil {
		log.Error("Unable to get git repository for: %s/%s Error: %v", ownerName, repoName, err)
		ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"err": err.Error(),
		})
		return
	}
	defer gitRepo.Close()

	// Generate git environment for checking commits
	env := os.Environ()
	if opts.GitAlternativeObjectDirectories != "" {
		env = append(env,
			private.GitAlternativeObjectDirectories+"="+opts.GitAlternativeObjectDirectories)
	}
	if opts.GitObjectDirectory != "" {
		env = append(env,
			private.GitObjectDirectory+"="+opts.GitObjectDirectory)
	}
	if opts.GitQuarantinePath != "" {
		env = append(env,
			private.GitQuarantinePath+"="+opts.GitQuarantinePath)
	}

	for i := range opts.OldCommitIDs {
		oldCommitID := opts.OldCommitIDs[i]
		newCommitID := opts.NewCommitIDs[i]
		refFullName := opts.RefFullNames[i]

		branchName := strings.TrimPrefix(refFullName, git.BranchPrefix)
		protectBranch, err := models.GetProtectedBranchBy(repo.ID, branchName)
		if err != nil {
			log.Error("Unable to get protected branch: %s in %-v Error: %v", branchName, repo, err)
			ctx.JSON(500, map[string]interface{}{
				"err": err.Error(),
			})
			return
		}
		if protectBranch != nil && protectBranch.IsProtected() {
			// detect and prevent deletion
			if newCommitID == git.EmptySHA {
				log.Warn("Forbidden: Branch: %s in %-v is protected from deletion", branchName, repo)
				ctx.JSON(http.StatusForbidden, map[string]interface{}{
					"err": fmt.Sprintf("branch %s is protected from deletion", branchName),
				})
				return
			}

			// detect force push
			if git.EmptySHA != oldCommitID {
				output, err := git.NewCommand("rev-list", "--max-count=1", oldCommitID, "^"+newCommitID).RunInDirWithEnv(repo.RepoPath(), env)
				if err != nil {
					log.Error("Unable to detect force push between: %s and %s in %-v Error: %v", oldCommitID, newCommitID, repo, err)
					ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
						"err": fmt.Sprintf("Fail to detect force push: %v", err),
					})
					return
				} else if len(output) > 0 {
					log.Warn("Forbidden: Branch: %s in %-v is protected from force push", branchName, repo)
					ctx.JSON(http.StatusForbidden, map[string]interface{}{
						"err": fmt.Sprintf("branch %s is protected from force push", branchName),
					})
					return

				}
			}

			// Require signed commits
			if protectBranch.RequireSignedCommits {
				err := verifyCommits(oldCommitID, newCommitID, gitRepo, env)
				if err != nil {
					if !isErrUnverifiedCommit(err) {
						log.Error("Unable to check commits from %s to %s in %-v: %v", oldCommitID, newCommitID, repo, err)
						ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
							"err": fmt.Sprintf("Unable to check commits from %s to %s: %v", oldCommitID, newCommitID, err),
						})
						return
					}
					unverifiedCommit := err.(*errUnverifiedCommit).sha
					log.Warn("Forbidden: Branch: %s in %-v is protected from unverified commit %s", branchName, repo, unverifiedCommit)
					ctx.JSON(http.StatusForbidden, map[string]interface{}{
						"err": fmt.Sprintf("branch %s is protected from unverified commit %s", branchName, unverifiedCommit),
					})
					return
				}
			}

			globs := protectBranch.GetProtectedFilePatterns()
			if len(globs) > 0 {
				err := checkFileProtection(oldCommitID, newCommitID, globs, gitRepo, env)
				if err != nil {
					if !models.IsErrFilePathProtected(err) {
						log.Error("Unable to check file protection for commits from %s to %s in %-v: %v", oldCommitID, newCommitID, repo, err)
						ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
							"err": fmt.Sprintf("Unable to check file protection for commits from %s to %s: %v", oldCommitID, newCommitID, err),
						})
						return
					}
					protectedFilePath := err.(models.ErrFilePathProtected).Path
					log.Warn("Forbidden: Branch: %s in %-v is protected from changing file %s", branchName, repo, protectedFilePath)
					ctx.JSON(http.StatusForbidden, map[string]interface{}{
						"err": fmt.Sprintf("branch %s is protected from changing file %s", branchName, protectedFilePath),
					})
					return
				}
			}

			canPush := false
			if opts.IsDeployKey {
				canPush = protectBranch.CanPush && (!protectBranch.EnableWhitelist || protectBranch.WhitelistDeployKeys)
			} else {
				canPush = protectBranch.CanUserPush(opts.UserID)
			}
			if !canPush && opts.ProtectedBranchID > 0 {
				// Merge (from UI or API)
				pr, err := models.GetPullRequestByID(opts.ProtectedBranchID)
				if err != nil {
					log.Error("Unable to get PullRequest %d Error: %v", opts.ProtectedBranchID, err)
					ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
						"err": fmt.Sprintf("Unable to get PullRequest %d Error: %v", opts.ProtectedBranchID, err),
					})
					return
				}
				user, err := models.GetUserByID(opts.UserID)
				if err != nil {
					log.Error("Unable to get User id %d Error: %v", opts.UserID, err)
					ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
						"err": fmt.Sprintf("Unable to get User id %d Error: %v", opts.UserID, err),
					})
					return
				}
				perm, err := models.GetUserRepoPermission(repo, user)
				if err != nil {
					log.Error("Unable to get Repo permission of repo %s/%s of User %s", repo.OwnerName, repo.Name, user.Name, err)
					ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
						"err": fmt.Sprintf("Unable to get Repo permission of repo %s/%s of User %s: %v", repo.OwnerName, repo.Name, user.Name, err),
					})
					return
				}
				allowedMerge, err := pull_service.IsUserAllowedToMerge(pr, perm, user)
				if err != nil {
					log.Error("Error calculating if allowed to merge: %v", err)
					ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
						"err": fmt.Sprintf("Error calculating if allowed to merge: %v", err),
					})
					return
				}
				if !allowedMerge {
					log.Warn("Forbidden: User %d is not allowed to push to protected branch: %s in %-v and is not allowed to merge pr #%d", opts.UserID, branchName, repo, pr.Index)
					ctx.JSON(http.StatusForbidden, map[string]interface{}{
						"err": fmt.Sprintf("Not allowed to push to protected branch %s", branchName),
					})
					return
				}
				// Check all status checks and reviews is ok, unless repo admin which can bypass this.
				if !perm.IsAdmin() {
					if err := pull_service.CheckPRReadyToMerge(pr); err != nil {
						if models.IsErrNotAllowedToMerge(err) {
							log.Warn("Forbidden: User %d is not allowed push to protected branch %s in %-v and pr #%d is not ready to be merged: %s", opts.UserID, branchName, repo, pr.Index, err.Error())
							ctx.JSON(http.StatusForbidden, map[string]interface{}{
								"err": fmt.Sprintf("Not allowed to push to protected branch %s and pr #%d is not ready to be merged: %s", branchName, opts.ProtectedBranchID, err.Error()),
							})
							return
						}
						log.Error("Unable to check if mergable: protected branch %s in %-v and pr #%d. Error: %v", opts.UserID, branchName, repo, pr.Index, err)
						ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
							"err": fmt.Sprintf("Unable to get status of pull request %d. Error: %v", opts.ProtectedBranchID, err),
						})
					}
				}
			} else if !canPush {
				log.Warn("Forbidden: User %d is not allowed to push to protected branch: %s in %-v", opts.UserID, branchName, repo)
				ctx.JSON(http.StatusForbidden, map[string]interface{}{
					"err": fmt.Sprintf("Not allowed to push to protected branch %s", branchName),
				})
				return
			}
		}
	}

	ctx.PlainText(http.StatusOK, []byte("ok"))
}

// HookPostReceive updates services and users
func HookPostReceive(ctx *macaron.Context, opts private.HookOptions) {
	ownerName := ctx.Params(":owner")
	repoName := ctx.Params(":repo")

	var repo *models.Repository
	updates := make([]*repofiles.PushUpdateOptions, 0, len(opts.OldCommitIDs))
	wasEmpty := false

	for i := range opts.OldCommitIDs {
		refFullName := opts.RefFullNames[i]

		// Only trigger activity updates for changes to branches or
		// tags.  Updates to other refs (eg, refs/notes, refs/changes,
		// or other less-standard refs spaces are ignored since there
		// may be a very large number of them).
		if strings.HasPrefix(refFullName, git.BranchPrefix) || strings.HasPrefix(refFullName, git.TagPrefix) {
			if repo == nil {
				var err error
				repo, err = models.GetRepositoryByOwnerAndName(ownerName, repoName)
				if err != nil {
					log.Error("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err)
					ctx.JSON(http.StatusInternalServerError, private.HookPostReceiveResult{
						Err: fmt.Sprintf("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err),
					})
					return
				}
				if repo.OwnerName == "" {
					repo.OwnerName = ownerName
				}
				wasEmpty = repo.IsEmpty
			}

			option := repofiles.PushUpdateOptions{
				RefFullName:  refFullName,
				OldCommitID:  opts.OldCommitIDs[i],
				NewCommitID:  opts.NewCommitIDs[i],
				PusherID:     opts.UserID,
				PusherName:   opts.UserName,
				RepoUserName: ownerName,
				RepoName:     repoName,
			}
			updates = append(updates, &option)
			if repo.IsEmpty && option.IsBranch() && option.BranchName() == "master" {
				// put the master branch first
				copy(updates[1:], updates)
				updates[0] = &option
			}
		}
	}

	if repo != nil && len(updates) > 0 {
		if err := repofiles.PushUpdates(repo, updates); err != nil {
			log.Error("Failed to Update: %s/%s Total Updates: %d", ownerName, repoName, len(updates))
			for i, update := range updates {
				log.Error("Failed to Update: %s/%s Update: %d/%d: Branch: %s", ownerName, repoName, i, len(updates), update.BranchName())
			}
			log.Error("Failed to Update: %s/%s Error: %v", ownerName, repoName, err)

			ctx.JSON(http.StatusInternalServerError, private.HookPostReceiveResult{
				Err: fmt.Sprintf("Failed to Update: %s/%s Error: %v", ownerName, repoName, err),
			})
			return
		}
	}

	results := make([]private.HookPostReceiveBranchResult, 0, len(opts.OldCommitIDs))

	// We have to reload the repo in case its state is changed above
	repo = nil
	var baseRepo *models.Repository

	for i := range opts.OldCommitIDs {
		refFullName := opts.RefFullNames[i]
		newCommitID := opts.NewCommitIDs[i]

		branch := git.RefEndName(opts.RefFullNames[i])

		if newCommitID != git.EmptySHA && strings.HasPrefix(refFullName, git.BranchPrefix) {
			if repo == nil {
				var err error
				repo, err = models.GetRepositoryByOwnerAndName(ownerName, repoName)
				if err != nil {
					log.Error("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err)
					ctx.JSON(http.StatusInternalServerError, private.HookPostReceiveResult{
						Err:          fmt.Sprintf("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err),
						RepoWasEmpty: wasEmpty,
					})
					return
				}
				if repo.OwnerName == "" {
					repo.OwnerName = ownerName
				}

				if !repo.AllowsPulls() {
					// We can stop there's no need to go any further
					ctx.JSON(http.StatusOK, private.HookPostReceiveResult{
						RepoWasEmpty: wasEmpty,
					})
					return
				}
				baseRepo = repo

				if repo.IsFork {
					if err := repo.GetBaseRepo(); err != nil {
						log.Error("Failed to get Base Repository of Forked repository: %-v Error: %v", repo, err)
						ctx.JSON(http.StatusInternalServerError, private.HookPostReceiveResult{
							Err:          fmt.Sprintf("Failed to get Base Repository of Forked repository: %-v Error: %v", repo, err),
							RepoWasEmpty: wasEmpty,
						})
						return
					}
					baseRepo = repo.BaseRepo
				}
			}

			if !repo.IsFork && branch == baseRepo.DefaultBranch {
				results = append(results, private.HookPostReceiveBranchResult{})
				continue
			}

			pr, err := models.GetUnmergedPullRequest(repo.ID, baseRepo.ID, branch, baseRepo.DefaultBranch)
			if err != nil && !models.IsErrPullRequestNotExist(err) {
				log.Error("Failed to get active PR in: %-v Branch: %s to: %-v Branch: %s Error: %v", repo, branch, baseRepo, baseRepo.DefaultBranch, err)
				ctx.JSON(http.StatusInternalServerError, private.HookPostReceiveResult{
					Err: fmt.Sprintf(
						"Failed to get active PR in: %-v Branch: %s to: %-v Branch: %s Error: %v", repo, branch, baseRepo, baseRepo.DefaultBranch, err),
					RepoWasEmpty: wasEmpty,
				})
				return
			}

			if pr == nil {
				if repo.IsFork {
					branch = fmt.Sprintf("%s:%s", repo.OwnerName, branch)
				}
				results = append(results, private.HookPostReceiveBranchResult{
					Message: setting.Git.PullRequestPushMessage && repo.AllowsPulls(),
					Create:  true,
					Branch:  branch,
					URL:     fmt.Sprintf("%s/compare/%s...%s", baseRepo.HTMLURL(), util.PathEscapeSegments(baseRepo.DefaultBranch), util.PathEscapeSegments(branch)),
				})
			} else {
				results = append(results, private.HookPostReceiveBranchResult{
					Message: setting.Git.PullRequestPushMessage && repo.AllowsPulls(),
					Create:  false,
					Branch:  branch,
					URL:     fmt.Sprintf("%s/pulls/%d", baseRepo.HTMLURL(), pr.Index),
				})
			}
		}
	}
	ctx.JSON(http.StatusOK, private.HookPostReceiveResult{
		Results:      results,
		RepoWasEmpty: wasEmpty,
	})
}

// SetDefaultBranch updates the default branch
func SetDefaultBranch(ctx *macaron.Context) {
	ownerName := ctx.Params(":owner")
	repoName := ctx.Params(":repo")
	branch := ctx.Params(":branch")
	repo, err := models.GetRepositoryByOwnerAndName(ownerName, repoName)
	if err != nil {
		log.Error("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err)
		ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"Err": fmt.Sprintf("Failed to get repository: %s/%s Error: %v", ownerName, repoName, err),
		})
		return
	}
	if repo.OwnerName == "" {
		repo.OwnerName = ownerName
	}

	repo.DefaultBranch = branch
	gitRepo, err := git.OpenRepository(repo.RepoPath())
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"Err": fmt.Sprintf("Failed to get git repository: %s/%s Error: %v", ownerName, repoName, err),
		})
		return
	}
	if err := gitRepo.SetDefaultBranch(repo.DefaultBranch); err != nil {
		if !git.IsErrUnsupportedVersion(err) {
			gitRepo.Close()
			ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"Err": fmt.Sprintf("Unable to set default branch onrepository: %s/%s Error: %v", ownerName, repoName, err),
			})
			return
		}
	}
	gitRepo.Close()

	if err := repo.UpdateDefaultBranch(); err != nil {
		ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"Err": fmt.Sprintf("Unable to set default branch onrepository: %s/%s Error: %v", ownerName, repoName, err),
		})
		return
	}
	ctx.PlainText(200, []byte("success"))
}
