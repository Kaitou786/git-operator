package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/flanksource/commons/console"
	"github.com/flanksource/commons/utils"
	gitv1 "github.com/flanksource/git-operator/api/v1"
	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	crdclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

const (
	namespace           = "platform-system"
	deployment          = "git-operator-controller-manager"
	repository          = "flanksource/git-operator-test"
	owner               = "flanksource"
	repoName            = "git-operator-test"
	pullRequestUsername = "teodor-pripoae"
)

var (
	k8s    *kubernetes.Clientset
	crdK8s crdclient.Client
	tests  = map[string]Test{
		"git-operator-is-running": TestGitOperatorIsRunning,
		// "github-branch-sync":      TestGithubBranchSync,
		"github-pr-github-sync": TestGithubPRSync,
	}
	scheme     = runtime.NewScheme()
	restConfig *rest.Config
)

type Test func(context.Context, *console.TestResults) error
type DeferFunc func()

func main() {
	var kubeconfig *string
	var timeout *time.Duration
	var err error
	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	timeout = flag.Duration("timeout", 5*time.Minute, "Global timeout for all tests")
	flag.Parse()

	_ = clientgoscheme.AddToScheme(scheme)

	_ = gitv1.AddToScheme(scheme)

	// use the current context in kubeconfig
	restConfig, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("failed to create k8s config: %v", err)
	}

	k8s, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("failed to create clientset: %v", err)
	}

	mapper, err := apiutil.NewDynamicRESTMapper(restConfig)
	if err != nil {
		log.Fatalf("failed to create mapper: %v", err)
	}
	crdK8s, err = crdclient.New(restConfig, crdclient.Options{Scheme: scheme, Mapper: mapper})

	test := &console.TestResults{
		Writer: os.Stdout,
	}

	errors := map[string]error{}
	deadline, _ := context.WithTimeout(context.Background(), *timeout)

	for name, t := range tests {
		err := t(deadline, test)
		if err != nil {
			errors[name] = err
		}
	}

	if len(errors) > 0 {
		for name, err := range errors {
			log.Errorf("test %s failed: %v", name, err)
		}
		os.Exit(1)
	}

	log.Infof("All tests passed !!!")
}

func TestGitOperatorIsRunning(ctx context.Context, test *console.TestResults) error {
	pods, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "control-plane=git-operator"})
	if err != nil {
		test.Failf("TestGitOperatorIsRunning", "failed to list git-operator pods: %v", err)
		return err
	}
	if len(pods.Items) != 1 {
		test.Failf("TestGitOperatorIsRunning", "expected 1 pod got %d", len(pods.Items))
		return errors.Errorf("Expected 1 pod got %d", len(pods.Items))
	}
	test.Passf("TestGitOperatorIsRunning", "%s pod is running", pods.Items[0].Name)
	return nil
}

func TestGithubBranchSync(ctx context.Context, test *console.TestResults) error {
	branchName := getBranchName("test-github-branch-sync")

	newSha, deferFunc, err := createNewBranch(ctx, test, branchName)
	defer deferFunc()
	if err != nil {
		test.Failf("TestGithubBranchSync", "failed to create branch %s: %v", branchName, err)
		return err
	}
	test.Passf("TestGithubBranchSync", "Successfully created branch %s", branchName)

	gitBranchGetCtx, _ := context.WithTimeout(ctx, 1*time.Minute)
	crdName := fmt.Sprintf("gitrepository-sample-%s", branchName)
	gitBranch, err := waitForGitBranch(gitBranchGetCtx, crdName)
	if err != nil {
		test.Failf("TestGithubBranchSync", "error waiting for branch %s", crdName)
		return err
	}
	if err := assertEquals(test, "TestGithubBranchSync", gitBranch.Labels["git.flanksource.com/repository"], "gitrepository-sample"); err != nil {
		return err
	}
	if err := assertEquals(test, "TestGithubBranchSync", gitBranch.Labels["git.flanksource.com/branch"], branchName); err != nil {
		return err
	}
	if err := assertEquals(test, "TestGithubBranchSync", gitBranch.Spec.BranchName, branchName); err != nil {
		return err
	}
	if err := assertEquals(test, "TestGithubBranchSync", gitBranch.Spec.Repository, repository); err != nil {
		return err
	}
	if err := assertEquals(test, "TestGithubBranchSync", gitBranch.Status.Head, newSha); err != nil {
		return err
	}

	test.Passf("TestGithubBranchSync", "Successfully checked GitBranch %s", crdName)

	return nil
}

func TestGithubPRSync(ctx context.Context, test *console.TestResults) error {
	branchName := getBranchName("test-github-pr-sync")

	newSha, deferFunc, err := createNewBranch(ctx, test, branchName)
	defer deferFunc()
	if err != nil {
		test.Failf("TestGithubPRSync", "failed to create branch %s: %v", branchName, err)
		return err
	}

	githubClient, err := githubClient(ctx)
	if err != nil {
		test.Failf("TestGithubPRSync", "failed to get github client: %v", err)
		return err
	}

	pull := &github.NewPullRequest{
		Title: github.String(fmt.Sprintf("[E2E] Test Pull Request sync from Github to CRD %s", branchName)),
		Head:  github.String(branchName),
		Base:  github.String("master"),
		Body:  github.String("This PullRequest is automatically generated by E2E suite."),
	}
	pr, _, err := githubClient.PullRequests.Create(ctx, owner, repoName, pull)

	gitPRGetCtx, _ := context.WithTimeout(ctx, 1*time.Minute)
	crdName := fmt.Sprintf("gitrepository-sample-%d", *pr.Number)
	gitPR, err := waitForGitPullRequest(gitPRGetCtx, crdName)
	if err != nil {
		test.Failf("TestGithubPRSync", "error waiting for PR %s", crdName)
		return err
	}

	prSpec := gitv1.GitPullRequestSpec{
		Base:       "master",
		Body:       *pull.Body,
		Fork:       "flanksource/git-operator-test",
		Head:       branchName,
		ID:         strconv.Itoa(*pr.Number),
		Ref:        fmt.Sprintf("refs/pull/%d/head", *pr.Number),
		Repository: "flanksource/git-operator-test",
		SHA:        newSha,
		Title:      *pull.Title,
	}

	if err := assertInterfaceEquals(test, "TestGithubPRSync", gitPR.Spec, prSpec); err != nil {
		return err
	}

	prStatus := &gitv1.GitPullRequestStatus{
		Author: pullRequestUsername,
		URL:    fmt.Sprintf("https://github.com/%s/pull/%d.diff", repository, *pr.Number),
	}

	if err := assertInterfaceEquals(test, "TestGithubPRSync", gitPR.Status, prStatus); err != nil {
		return err
	}

	test.Passf("TestGithubPRSync", "Successfully checked GitPullRequest %s", crdName)

	return nil
}

func TestGithubPRCRDSync(ctx context.Context, test *console.TestResults) error {
	branchName := getBranchName("test-github-pr-crd-sync")

	newSha, deferFunc, err := createNewBranch(ctx, test, branchName)
	defer deferFunc()
	if err != nil {
		test.Failf("TestGithubPRCRDSync", "failed to create branch %s: %v", branchName, err)
		return err
	}

	githubClient, err := githubClient(ctx)
	if err != nil {
		test.Failf("TestGithubPRCRDSync", "failed to get github client: %v", err)
		return err
	}

	opts := &github.PullRequestListOptions{
		State: "all",
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	}
	prList, _, err := githubClient.PullRequests.List(ctx, owner, repoName, opts)
	if err != nil {
		test.Failf("TestGithubPRCRDSync", "failed to list github pull requests: %v", err)
	}
	latestPRCount := 1
	if len(prList) > 0 {
		latestPRCount = *prList[0].Number + 1
	}

	crdName := fmt.Sprintf("gitrepository-sample-%d", latestPRCount)
	gitPRCRD := &gitv1.GitPullRequest{
		TypeMeta: metav1.TypeMeta{APIVersion: "git.flanksource.com/v1", Kind: "GitPullRequest"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      crdName,
			Namespace: namespace,
			Labels: map[string]string{
				"git.flanksource.com/repository": "gitrepository-sample",
			},
		},
		Spec: gitv1.GitPullRequestSpec{
			Base:       "master",
			Body:       "This PullRequest is automatically generated by E2E suite from GitPullRequest CRD.",
			Fork:       repository,
			Head:       branchName,
			ID:         strconv.Itoa(latestPRCount),
			Ref:        fmt.Sprintf("refs/pull/%d/head", latestPRCount),
			Repository: repository,
			SHA:        newSha,
			Title:      fmt.Sprintf("[E2E] Test Pull Request sync from CRD to Github %s", branchName),
		},
		Status: gitv1.GitPullRequestStatus{
			Author: pullRequestUsername,
			URL:    fmt.Sprintf("https://github.com/%s/pull/%d.diff", repository, latestPRCount),
		},
	}

	if err := crdK8s.Create(ctx, gitPRCRD); err != nil {

	}

	gitPRGetCtx, _ := context.WithTimeout(ctx, 1*time.Minute)
	gitPR, err := waitForGitPullRequestFromCrd(gitPRGetCtx, latestPRCount)
	if err != nil {
		test.Failf("TestGithubPRCRDSync", "error waiting for PR to be created in Github %s", latestPRCount)
		return err
	}

	test.Passf("TestGithubPRCRDSync", "Successfully checked GitPullRequest %s", crdName)

	return nil
}

func waitForGitBranch(ctx context.Context, crdName string) (*gitv1.GitBranch, error) {
	gitBranch := &gitv1.GitBranch{}
	for {
		err := crdK8s.Get(ctx, types.NamespacedName{Name: crdName, Namespace: namespace}, gitBranch)
		if err != nil {
			if kerrors.IsNotFound(err) {
				log.Debugf("GitBranch %s in namespace %s does not exist", crdName, namespace)
				time.Sleep(2 * time.Second)
				continue
			}
			return nil, errors.Wrapf(err, "failed to get GitBranch %s in namespace %s", crdName, namespace)
		}
		return gitBranch, nil
	}
}

func waitForGitPullRequest(ctx context.Context, crdName string) (*gitv1.GitPullRequest, error) {
	gitPR := &gitv1.GitPullRequest{}
	for {
		err := crdK8s.Get(ctx, types.NamespacedName{Name: crdName, Namespace: namespace}, gitPR)
		if err != nil {
			if kerrors.IsNotFound(err) {
				log.Debugf("GitPullRequest %s in namespace %s does not exist", crdName, namespace)
				time.Sleep(2 * time.Second)
				continue
			}
			return nil, errors.Wrapf(err, "failed to get GitPullRequest %s in namespace %s", crdName, namespace)
		}
		return gitPR, nil
	}
}

func createNewBranch(ctx context.Context, test *console.TestResults, branchName string) (string, DeferFunc, error) {
	noop := func() {}
	githubClient, err := githubClient(ctx)
	if err != nil {
		return "", noop, err
	}

	masterBranch, _, err := githubClient.Git.GetRef(ctx, owner, repoName, "refs/heads/master")
	if err != nil {
		return "", noop, errors.Wrap(err, "failed to get master ref")
	}

	refName := fmt.Sprintf("refs/heads/%s", branchName)
	ref := &github.Reference{
		Ref: &refName,
		Object: &github.GitObject{
			SHA: masterBranch.Object.SHA,
		},
	}
	_, _, err = githubClient.Git.CreateRef(ctx, owner, repoName, ref)
	if err != nil {
		return "", noop, errors.Wrap(err, "failed to create ref")
	}

	opts := &github.RepositoryContentFileOptions{
		Message: github.String("Updated by E2E test TestGithubRepositoryClone"),
		Content: []byte(fmt.Sprintf("# The content from branch %s", branchName)),
		Branch:  &branchName,
		Author: &github.CommitAuthor{
			Name:  github.String("Flanksource bot"),
			Email: github.String("github.bot@flanksource.com"),
		},
	}
	fileResponse, _, err := githubClient.Repositories.CreateFile(ctx, owner, repoName, "files/foo.txt", opts)
	if err != nil {
		return "", noop, errors.Wrap(err, "failed to create file")
	}

	newSha := fileResponse.Commit.SHA

	deferFunc := func() {
		_, err = githubClient.Git.DeleteRef(ctx, owner, repoName, refName)
		if err != nil {
			test.Failf("TestGithubBranchSync", "failed to delete ref %s", refName)
		}
	}

	return *newSha, deferFunc, nil
}

func assertEquals(test *console.TestResults, name, actual, expected string) error {
	if actual != expected {
		test.Failf(name, "expected %s to equal %s", actual, expected)
		return errors.Errorf("Test %s expected %s to equal %s", name, actual, expected)
	}
	return nil
}

func assertInterfaceEquals(test *console.TestResults, name string, actual, expected interface{}) error {
	actualYml, err := yaml.Marshal(actual)
	if err != nil {
		return errors.Wrap(err, "failed to marshal actual")
	}

	expectedYml, err := yaml.Marshal(expected)
	if err != nil {
		return errors.Wrap(err, "failed to marshal expected")
	}

	if string(actualYml) != string(expectedYml) {
		test.Failf("Expected: %s\n%s\n\nTo Equal:\n%s\n", string(actualYml), string(expectedYml))
		return errors.Errorf("Test %s expected:\n%s\nTo Match:\n%s\n", name, actualYml, expectedYml)
	}

	return nil
}

func getBranchName(baseName string) string {
	date := time.Now().Format("20060201150405")
	hash := utils.RandomString(4)
	return fmt.Sprintf("%s-%s-%s", baseName, date, hash)
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func githubClient(ctx context.Context) (*github.Client, error) {
	authToken := os.Getenv("GITHUB_TOKEN")
	if authToken == "" {
		return nil, errors.New("GITHUB_TOKEN not provided")
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: authToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	return client, nil
}
