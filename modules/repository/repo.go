// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/lfs"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"
)

/*
GitHub, GitLab, Gogs: *.wiki.git
BitBucket: *.git/wiki
*/
var commonWikiURLSuffixes = []string{".wiki.git", ".git/wiki"}

// WikiRemoteURL returns accessible repository URL for wiki if exists.
// Otherwise, it returns an empty string.
func WikiRemoteURL(ctx context.Context, remote string) string {
	remote = strings.TrimSuffix(remote, ".git")
	for _, suffix := range commonWikiURLSuffixes {
		wikiURL := remote + suffix
		if git.IsRepoURLAccessible(ctx, wikiURL) {
			return wikiURL
		}
	}
	return ""
}

// SyncRepoTags synchronizes releases table with repository tags
func SyncRepoTags(ctx context.Context, repoID int64) error {
	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return err
	}

	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	if err != nil {
		return err
	}
	defer gitRepo.Close()

	return SyncReleasesWithTags(ctx, repo, gitRepo)
}

// StoreMissingLfsObjectsInRepository downloads missing LFS objects
func StoreMissingLfsObjectsInRepository(ctx context.Context, repo *repo_model.Repository, gitRepo *git.Repository, lfsClient lfs.Client) error {
	contentStore := lfs.NewContentStore()

	pointerChan := make(chan lfs.PointerBlob)
	errChan := make(chan error, 1)
	go lfs.SearchPointerBlobs(ctx, gitRepo, pointerChan, errChan)

	downloadObjects := func(pointers []lfs.Pointer) error {
		err := lfsClient.Download(ctx, pointers, func(p lfs.Pointer, content io.ReadCloser, objectError error) error {
			if errors.Is(objectError, lfs.ErrObjectNotExist) {
				log.Warn("Ignoring missing upstream LFS object %-v: %v", p, objectError)
				return nil
			}

			if objectError != nil {
				return objectError
			}

			defer content.Close()

			_, err := git_model.NewLFSMetaObject(ctx, repo.ID, p)
			if err != nil {
				log.Error("Repo[%-v]: Error creating LFS meta object %-v: %v", repo, p, err)
				return err
			}

			if err := contentStore.Put(p, content); err != nil {
				log.Error("Repo[%-v]: Error storing content for LFS meta object %-v: %v", repo, p, err)
				if _, err2 := git_model.RemoveLFSMetaObjectByOid(ctx, repo.ID, p.Oid); err2 != nil {
					log.Error("Repo[%-v]: Error removing LFS meta object %-v: %v", repo, p, err2)
				}
				return err
			}
			return nil
		})
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}
		return err
	}

	var batch []lfs.Pointer
	for pointerBlob := range pointerChan {
		meta, err := git_model.GetLFSMetaObjectByOid(ctx, repo.ID, pointerBlob.Oid)
		if err != nil && err != git_model.ErrLFSObjectNotExist {
			log.Error("Repo[%-v]: Error querying LFS meta object %-v: %v", repo, pointerBlob.Pointer, err)
			return err
		}
		if meta != nil {
			log.Trace("Repo[%-v]: Skipping unknown LFS meta object %-v", repo, pointerBlob.Pointer)
			continue
		}

		log.Trace("Repo[%-v]: LFS object %-v not present in repository", repo, pointerBlob.Pointer)

		exist, err := contentStore.Exists(pointerBlob.Pointer)
		if err != nil {
			log.Error("Repo[%-v]: Error checking if LFS object %-v exists: %v", repo, pointerBlob.Pointer, err)
			return err
		}

		if exist {
			log.Trace("Repo[%-v]: LFS object %-v already present; creating meta object", repo, pointerBlob.Pointer)
			_, err := git_model.NewLFSMetaObject(ctx, repo.ID, pointerBlob.Pointer)
			if err != nil {
				log.Error("Repo[%-v]: Error creating LFS meta object %-v: %v", repo, pointerBlob.Pointer, err)
				return err
			}
		} else {
			if setting.LFS.MaxFileSize > 0 && pointerBlob.Size > setting.LFS.MaxFileSize {
				log.Info("Repo[%-v]: LFS object %-v download denied because of LFS_MAX_FILE_SIZE=%d < size %d", repo, pointerBlob.Pointer, setting.LFS.MaxFileSize, pointerBlob.Size)
				continue
			}

			batch = append(batch, pointerBlob.Pointer)
			if len(batch) >= lfsClient.BatchSize() {
				if err := downloadObjects(batch); err != nil {
					return err
				}
				batch = nil
			}
		}
	}
	if len(batch) > 0 {
		if err := downloadObjects(batch); err != nil {
			return err
		}
	}

	err, has := <-errChan
	if has {
		log.Error("Repo[%-v]: Error enumerating LFS objects for repository: %v", repo, err)
		return err
	}

	return nil
}

// shortRelease to reduce load memory, this struct can replace repo_model.Release
type shortRelease struct {
	ID      int64
	TagName string
	Sha1    string
	IsTag   bool
}

func (shortRelease) TableName() string {
	return "release"
}

// SyncReleasesWithTags is a tag<->release table
// synchronization which overwrites all Releases from the repository tags. This
// can be relied on since a pull-mirror is always identical to its
// upstream. Hence, after each sync we want the release set to be
// identical to the upstream tag set. This is much more efficient for
// repositories like https://github.com/vim/vim (with over 13000 tags).
func SyncReleasesWithTags(ctx context.Context, repo *repo_model.Repository, gitRepo *git.Repository) error {
	log.Debug("SyncReleasesWithTags: in Repo[%d:%s/%s]", repo.ID, repo.OwnerName, repo.Name)
	tags, _, err := gitRepo.GetTagInfos(0, 0)
	if err != nil {
		return fmt.Errorf("unable to GetTagInfos in pull-mirror Repo[%d:%s/%s]: %w", repo.ID, repo.OwnerName, repo.Name, err)
	}
	var added, deleted, updated int
	err = db.WithTx(ctx, func(ctx context.Context) error {
		dbReleases, err := db.Find[shortRelease](ctx, repo_model.FindReleasesOptions{
			RepoID:        repo.ID,
			IncludeDrafts: true,
			IncludeTags:   true,
		})
		if err != nil {
			return fmt.Errorf("unable to FindReleases in pull-mirror Repo[%d:%s/%s]: %w", repo.ID, repo.OwnerName, repo.Name, err)
		}

		inserts, deletes, updates := calcSync(tags, dbReleases)
		//
		// make release set identical to upstream tags
		//
		for _, tag := range inserts {
			release := repo_model.Release{
				RepoID:       repo.ID,
				TagName:      tag.Name,
				LowerTagName: strings.ToLower(tag.Name),
				Sha1:         tag.Object.String(),
				// NOTE: ignored, The NumCommits value is calculated and cached on demand when the UI requires it.
				NumCommits:  -1,
				CreatedUnix: timeutil.TimeStamp(tag.Tagger.When.Unix()),
				IsTag:       true,
			}
			if err := db.Insert(ctx, release); err != nil {
				return fmt.Errorf("unable insert tag %s for pull-mirror Repo[%d:%s/%s]: %w", tag.Name, repo.ID, repo.OwnerName, repo.Name, err)
			}
		}

		// only delete tags releases
		if len(deletes) > 0 {
			if _, err := db.GetEngine(ctx).Where("repo_id=?", repo.ID).
				In("id", deletes).
				Delete(&repo_model.Release{}); err != nil {
				return fmt.Errorf("unable to delete tags for pull-mirror Repo[%d:%s/%s]: %w", repo.ID, repo.OwnerName, repo.Name, err)
			}
		}

		for _, tag := range updates {
			if _, err := db.GetEngine(ctx).Where("repo_id = ? AND lower_tag_name = ?", repo.ID, strings.ToLower(tag.Name)).
				Cols("sha1", "created_unix").
				Update(&repo_model.Release{
					Sha1:        tag.Object.String(),
					CreatedUnix: timeutil.TimeStamp(tag.Tagger.When.Unix()),
				}); err != nil {
				return fmt.Errorf("unable to update tag %s for pull-mirror Repo[%d:%s/%s]: %w", tag.Name, repo.ID, repo.OwnerName, repo.Name, err)
			}
		}
		added, deleted, updated = len(deletes), len(updates), len(inserts)
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to rebuild release table for pull-mirror Repo[%d:%s/%s]: %w", repo.ID, repo.OwnerName, repo.Name, err)
	}

	log.Trace("SyncReleasesWithTags: %d tags added, %d tags deleted, %d tags updated", added, deleted, updated)
	return nil
}

func calcSync(destTags []*git.Tag, dbTags []*shortRelease) ([]*git.Tag, []int64, []*git.Tag) {
	destTagMap := make(map[string]*git.Tag)
	for _, tag := range destTags {
		destTagMap[tag.Name] = tag
	}
	dbTagMap := make(map[string]*shortRelease)
	for _, rel := range dbTags {
		dbTagMap[rel.TagName] = rel
	}

	inserted := make([]*git.Tag, 0, 10)
	updated := make([]*git.Tag, 0, 10)
	for _, tag := range destTags {
		rel := dbTagMap[tag.Name]
		if rel == nil {
			inserted = append(inserted, tag)
		} else if rel.Sha1 != tag.Object.String() {
			updated = append(updated, tag)
		}
	}
	deleted := make([]int64, 0, 10)
	for _, tag := range dbTags {
		if destTagMap[tag.TagName] == nil && tag.IsTag {
			deleted = append(deleted, tag.ID)
		}
	}
	return inserted, deleted, updated
}
