package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
)

// --- Upsert Multiple Files Function (safe & detailed) ---

func upsertMultipleFilesSafe(
	client *github.Client,
	owner, repo, branch string,
	files map[string]string,
	commitMessage string,
) (map[string]string, error) {
	ctx := context.Background()
	result := make(map[string]string)

	ref, _, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok && (ghErr.Response.StatusCode == 404 || ghErr.Response.StatusCode == 409) {
			log.Println("Branch doesn't exist — repo may be empty. Creating initial commit...")

			var treeEntries []*github.TreeEntry
			for path, content := range files {
				result[path] = "created"
				blob, _, err := client.Git.CreateBlob(ctx, owner, repo, &github.Blob{
					Content:  github.String(content),
					Encoding: github.String("utf-8"),
				})

				if err != nil {

					result[path] = "error"
					return result, fmt.Errorf("CreateBlob (init): %w", err)
				}
				treeEntries = append(treeEntries, &github.TreeEntry{
					Path: github.String(path),
					Mode: github.String("100644"),
					Type: github.String("blob"),
					SHA:  blob.SHA,
				})
			}

			tree, _, err := client.Git.CreateTree(ctx, owner, repo, "", treeEntries)
			if err != nil {
				return result, fmt.Errorf("CreateTree (init): %w", err)
			}

			commit := &github.Commit{
				Message: github.String("Initial commit"),
				Tree:    tree,
			}

			newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, commit)
			if err != nil {
				return result, fmt.Errorf("CreateCommit (init): %w", err)
			}

			ref := &github.Reference{
				Ref: github.String("refs/heads/" + branch),
				Object: &github.GitObject{
					SHA: newCommit.SHA,
				},
			}
			_, _, err = client.Git.CreateRef(ctx, owner, repo, ref)
			if err != nil {
				return result, fmt.Errorf("CreateRef (init): %w", err)
			}

			log.Println("Initial commit and branch created.")
			return result, nil
		}
		return result, fmt.Errorf("GetRef: %w", err)
	}

	originalHeadSHA := ref.Object.GetSHA()

	baseCommit, resp, err := client.Repositories.GetCommit(ctx, owner, repo, originalHeadSHA, nil)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := os.ReadFile(resp.Request.URL.Path)
			body = string(b)
		}
		return result, fmt.Errorf("GetCommit error: %w\nStatus: %v\nBody: %s", err, resp.Status, body)
	}

	if baseCommit == nil || baseCommit.Commit == nil {
		return result, fmt.Errorf("baseCommit or baseCommit.Commit is nil — SHA might be invalid or repo in bad state")
	}

	baseTreeSHA := baseCommit.Commit.Tree.GetSHA()

	var treeEntries []*github.TreeEntry

	for path, newContent := range files {
		result[path] = "error"

		current, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: branch})
		if resp != nil && resp.StatusCode == 404 {
			result[path] = "created"
		} else if err == nil && current != nil {
			decoded, err := current.GetContent()
			if err != nil {
				continue
			}
			if decoded == newContent {
				result[path] = "skipped"
				continue
			}
			result[path] = "updated"
		} else if err != nil {
			continue
		}

		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, &github.Blob{
			Content:  github.String(newContent),
			Encoding: github.String("utf-8"),
		})
		if err != nil {
			continue
		}

		treeEntries = append(treeEntries, &github.TreeEntry{
			Path: github.String(path),
			Mode: github.String("100644"),
			Type: github.String("blob"),
			SHA:  blob.SHA,
		})
	}

	if len(treeEntries) == 0 {
		fmt.Println("No changes to commit.")
		return result, nil
	}

	refCheck, _, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		return result, fmt.Errorf("Recheck GetRef: %w", err)
	}
	if refCheck.Object.GetSHA() != originalHeadSHA {
		return result, fmt.Errorf("Branch was updated during operation (SHA mismatch)")
	}

	newTree, _, err := client.Git.CreateTree(ctx, owner, repo, baseTreeSHA, treeEntries)
	if err != nil {
		return result, fmt.Errorf("CreateTree: %w", err)
	}

	if baseCommit.Commit == nil {
		return result, fmt.Errorf("baseCommit.Commit is nil, cannot create new commit")
	}

	newCommit := &github.Commit{
		Message: github.String(commitMessage),
		Tree:    newTree,
		Parents: []*github.Commit{
			{
				SHA: github.String(originalHeadSHA),
			},
		},
	}
	commit, _, err := client.Git.CreateCommit(ctx, owner, repo, newCommit)
	if err != nil {
		return result, fmt.Errorf("CreateCommit: %w", err)
	}

	ref.Object.SHA = commit.SHA
	_, _, err = client.Git.UpdateRef(ctx, owner, repo, ref, false)
	if err != nil {
		return result, fmt.Errorf("UpdateRef: %w", err)
	}

	fmt.Println("Commit created:", commit.GetHTMLURL())
	return result, nil
}

func createRepo(client *github.Client, owner, repoName string) error {
	ctx := context.Background()

	// Check if the repository already exists
	_, resp, err := client.Repositories.Get(ctx, owner, repoName)
	if err == nil {
		log.Println("Repo already exists:", fmt.Sprintf("https://github.com/%s/%s", owner, repoName))
		return nil
	}
	if resp != nil && resp.StatusCode != 404 {
		return fmt.Errorf("Error checking if repo exists: %w", err)
	}

	// Repo doesn't exist, so create it
	repo := &github.Repository{
		Name:        github.String(repoName),
		Private:     github.Bool(false),
		AutoInit:    github.Bool(true), // 🔑 This initializes repo with a README
		Description: github.String("Auto-created with Go script"),
	}

	createdRepo, _, err := client.Repositories.Create(ctx, "", repo)
	if err != nil {
		return fmt.Errorf("Error creating repo: %w", err)
	}

	log.Println("Repo created:", createdRepo.GetHTMLURL())
	return nil
}

func createInitialMainBranch(client *github.Client, owner, repo string, files map[string]string) error {
	ctx := context.Background()
	var treeEntries []*github.TreeEntry

	// Step 1: Create blobs for files
	for path, content := range files {
		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, &github.Blob{
			Content:  github.String(content),
			Encoding: github.String("utf-8"),
		})
		if err != nil {
			return fmt.Errorf("failed to create blob: %w", err)
		}

		treeEntries = append(treeEntries, &github.TreeEntry{
			Path: github.String(path),
			Mode: github.String("100644"),
			Type: github.String("blob"),
			SHA:  blob.SHA,
		})
	}

	// Step 2: Create the tree
	tree, _, err := client.Git.CreateTree(ctx, owner, repo, "", treeEntries)
	if err != nil {
		return fmt.Errorf("failed to create tree: %w", err)
	}

	// Step 3: Create the commit
	commit := &github.Commit{
		Message: github.String("Initial commit"),
		Tree:    tree,
	}
	newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, commit)
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	// Step 4: Create the "main" branch reference
	ref := &github.Reference{
		Ref: github.String("refs/heads/main"),
		Object: &github.GitObject{
			SHA: newCommit.SHA,
		},
	}
	_, _, err = client.Git.CreateRef(ctx, owner, repo, ref)
	if err != nil {
		return fmt.Errorf("failed to create ref: %w", err)
	}

	fmt.Println("✅ Created main branch with initial commit")
	return nil
}

func main() {

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN is not set in the environment")
	}

	owner := "Santosh-etailify" // change this
	repo := "gitapis10"         // change this
	branch := "main"            // change if needed
	commitMessage := "Upsert files from Go script"

	localFiles := []string{
		"main.go",
	}

	files := make(map[string]string)

	for _, localPath := range localFiles {
		content, err := os.ReadFile(localPath)
		if err != nil {
			log.Fatalf("Failed to read %s: %v", localPath, err)
		}

		// Use forward slashes even on Windows for GitHub paths
		//repoPath := filepath.ToSlash(localPath)

		files[localPath] = string(content)
	}

	// === GitHub Client ===
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// === Run Upsert ===
	err := createRepo(client, owner, repo)
	if err != nil {
		log.Fatalf("Failed to create repo: %v", err)
	}
	// err = createInitialMainBranch(client, owner, repo, files)
	// if err != nil {
	// 	log.Fatalf("❌ Error: %v", err)
	// }

	result, err := upsertMultipleFilesSafe(client, owner, repo, branch, files, commitMessage)
	if err != nil {
		log.Fatalf("Failed to upsert files: %v", err)
	}

	//=== Print Summary ===
	fmt.Println("File Update Summary:")
	for file, status := range result {
		fmt.Printf("  %s → %s\n", file, status)
	}
}
