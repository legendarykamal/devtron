package util

import (
	"fmt"
	"github.com/xanzy/go-gitlab"
	"go.uber.org/zap"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
)

type GitLabClient struct {
	client     *gitlab.Client
	config     *GitConfig
	logger     *zap.SugaredLogger
	gitService GitService
}

func NewGitLabClient(config *GitConfig, logger *zap.SugaredLogger, gitService GitService) (GitClient, error) {
	var gitLabClient *gitlab.Client
	var err error
	if len(config.GitHost) > 0 {
		_, err = url.ParseRequestURI(config.GitHost)
		if err != nil {
			return nil, err
		}
		gitLabClient, err = gitlab.NewClient(config.GitToken, gitlab.WithBaseURL(config.GitHost))
		if err != nil {
			return nil, err
		}
	} else {
		gitLabClient, err = gitlab.NewClient(config.GitToken)
		if err != nil {
			return nil, err
		}
	}

	gitlabGroupId := ""
	if len(config.GitlabGroupId) > 0 {
		if _, err := strconv.Atoi(config.GitlabGroupId); err == nil {
			gitlabGroupId = config.GitlabGroupId
		} else {
			groups, res, err := gitLabClient.Groups.SearchGroup(config.GitlabGroupId)
			if err != nil {
				responseStatus := 0
				if res != nil {
					responseStatus = res.StatusCode
				}
				logger.Warnw("error connecting to gitlab", "status code", responseStatus, "err", err.Error())
			}
			logger.Debugw("gitlab groups found ", "group", groups)
			if len(groups) == 0 {
				logger.Warn("no matching namespace found for gitlab")
			}
			for _, group := range groups {
				if config.GitlabGroupId == group.Name {
					gitlabGroupId = strconv.Itoa(group.ID)
				}
			}
		}
	} else {
		return nil, fmt.Errorf("no gitlab group id found")
	}
	if gitlabGroupId == "" {
		return nil, fmt.Errorf("no gitlab group id found")
	}
	group, _, err := gitLabClient.Groups.GetGroup(gitlabGroupId, &gitlab.GetGroupOptions{})
	if err != nil {
		return nil, err
	}
	if group != nil {
		config.GitlabGroupPath = group.FullPath
	}
	logger.Debugw("gitlab config", "config", config)
	return &GitLabClient{
		client:     gitLabClient,
		config:     config,
		logger:     logger,
		gitService: gitService,
	}, nil
}

func (impl GitLabClient) DeleteRepository(name string) error {
	err := impl.DeleteProject(name)
	if err != nil {
		impl.logger.Errorw("error in deleting repo gitlab", "project", name, "err", err)
	}
	return err
}
func (impl GitLabClient) CreateRepository(name, description, userName, userEmailId string) (url string, isNew bool, detailedErrorGitOpsConfigActions DetailedErrorGitOpsConfigActions) {
	detailedErrorGitOpsConfigActions.StageErrorMap = make(map[string]error)
	impl.logger.Debugw("gitlab app create request ", "name", name, "description", description)
	repoUrl, err := impl.GetRepoUrl(name)
	if err != nil {
		impl.logger.Errorw("error in getting repo url ", "gitlab project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[GetRepoUrlStage] = err
		return "", false, detailedErrorGitOpsConfigActions
	}
	if len(repoUrl) > 0 {
		detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, GetRepoUrlStage)
		return repoUrl, false, detailedErrorGitOpsConfigActions
	} else {
		url, err = impl.createProject(name, description)
		if err != nil {
			detailedErrorGitOpsConfigActions.StageErrorMap[CreateRepoStage] = err
			return "", true, detailedErrorGitOpsConfigActions
		}
		detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CreateRepoStage)
	}
	repoUrl = url
	validated, err := impl.ensureProjectAvailability(name)
	if err != nil {
		impl.logger.Errorw("error in ensuring project availability ", "gitlab project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneHttpStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	if !validated {
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneHttpStage] = fmt.Errorf("unable to validate project:%s in given time", name)
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CloneHttpStage)
	_, err = impl.CreateReadme(fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, name), userName, userEmailId)
	if err != nil {
		impl.logger.Errorw("error in creating readme ", "gitlab project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CreateReadmeStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CreateReadmeStage)
	validated, err = impl.ensureProjectAvailabilityOnSsh(name, repoUrl)
	if err != nil {
		impl.logger.Errorw("error in ensuring project availability ", "gitlab project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneSshStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	if !validated {
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneSshStage] = fmt.Errorf("unable to validate project:%s in given time", name)
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CloneSshStage)
	return url, true, detailedErrorGitOpsConfigActions
}

func (impl GitLabClient) DeleteProject(projectName string) (err error) {
	impl.logger.Infow("deleting project ", "gitlab project name", projectName)
	_, err = impl.client.Projects.DeleteProject(fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, projectName))
	return err
}
func (impl GitLabClient) createProject(name, description string) (url string, err error) {
	var namespace = impl.config.GitlabGroupId
	namespaceId, err := strconv.Atoi(namespace)
	if err != nil {
		return "", err
	}

	// Create new project
	p := &gitlab.CreateProjectOptions{
		Name:                 gitlab.String(name),
		Description:          gitlab.String(description),
		MergeRequestsEnabled: gitlab.Bool(true),
		SnippetsEnabled:      gitlab.Bool(false),
		Visibility:           gitlab.Visibility(gitlab.PrivateVisibility),
		NamespaceID:          &namespaceId,
	}
	project, _, err := impl.client.Projects.CreateProject(p)
	if err != nil {
		impl.logger.Errorw("err in creating gitlab app", "req", p, "name", name, "err", err)
		return "", err
	}
	impl.logger.Infow("gitlab app created", "name", name, "url", project.HTTPURLToRepo)
	return project.HTTPURLToRepo, nil
}

func (impl GitLabClient) ensureProjectAvailability(projectName string) (bool, error) {
	pid := fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, projectName)
	count := 0
	verified := false
	for count < 3 && !verified {
		count = count + 1
		_, res, err := impl.client.Projects.GetProject(pid, &gitlab.GetProjectOptions{})
		if err != nil {
			return verified, err
		}
		if res.StatusCode >= 200 && res.StatusCode <= 299 {
			verified = true
			return verified, nil
		}
		time.Sleep(10 * time.Second)
	}
	return false, nil
}

func (impl GitLabClient) ensureProjectAvailabilityOnSsh(projectName string, repoUrl string) (bool, error) {
	count := 0
	for count < 3 {
		count = count + 1
		_, err := impl.gitService.Clone(repoUrl, fmt.Sprintf("/ensure-clone/%s", projectName))
		if err == nil {
			impl.logger.Infow("gitlab ensureProjectAvailability clone passed", "try count", count, "repoUrl", repoUrl)
			return true, nil
		}
		if err != nil {
			impl.logger.Errorw("gitlab ensureProjectAvailability clone failed", "try count", count, "err", err)
		}
		time.Sleep(10 * time.Second)
	}
	return false, nil
}

func (impl GitLabClient) GetRepoUrl(projectName string) (repoUrl string, err error) {
	pid := fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, projectName)
	prop, res, err := impl.client.Projects.GetProject(pid, &gitlab.GetProjectOptions{})
	if err != nil {
		impl.logger.Debugw("gitlab get project err", "pid", pid, "err", err)
		if res != nil && res.StatusCode == 404 {
			return "", nil
		}
		return "", err
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return prop.HTTPURLToRepo, nil
	}
	return "", nil
}

func (impl GitLabClient) CreateReadme(name, userName, userEmailId string) (string, error) {
	fileAction := gitlab.FileCreate
	filePath := "README.md"
	fileContent := "devtron licence"
	actions := &gitlab.CreateCommitOptions{
		Branch:        gitlab.String("master"),
		CommitMessage: gitlab.String("test commit"),
		Actions:       []*gitlab.CommitActionOptions{{Action: &fileAction, FilePath: &filePath, Content: &fileContent}},
		AuthorEmail:   &userEmailId,
		AuthorName:    &userName,
	}
	c, _, err := impl.client.Commits.CreateCommit(name, actions)
	return c.ID, err
}
func (impl GitLabClient) checkIfFileExists(projectName, ref, file string) (exists bool, err error) {
	_, _, err = impl.client.RepositoryFiles.GetFileMetaData(fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, projectName), file, &gitlab.GetFileMetaDataOptions{Ref: &ref})
	return err == nil, err
}

func (impl GitLabClient) CommitValues(config *ChartConfig) (commitHash string, commitTime time.Time, err error) {
	branch := "master"
	path := filepath.Join(config.ChartLocation, config.FileName)
	exists, err := impl.checkIfFileExists(config.ChartRepoName, branch, path)
	var fileAction gitlab.FileActionValue
	if exists {
		fileAction = gitlab.FileUpdate
	} else {
		fileAction = gitlab.FileCreate
	}
	actions := &gitlab.CreateCommitOptions{
		Branch:        &branch,
		CommitMessage: gitlab.String(config.ReleaseMessage),
		Actions:       []*gitlab.CommitActionOptions{{Action: &fileAction, FilePath: &path, Content: &config.FileContent}},
		AuthorEmail:   &config.UserEmailId,
		AuthorName:    &config.UserName,
	}
	c, _, err := impl.client.Commits.CreateCommit(fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, config.ChartRepoName), actions)
	if err != nil {
		return "", time.Time{}, err
	}
	return c.ID, *c.AuthoredDate, err
}

func (impl GitLabClient) GetCommits(repoName, projectName string) ([]*GitCommitDto, error) {
	gitlabClient := impl.client
	branch := "master"
	listCommitOptions := &gitlab.ListCommitsOptions{
		RefName: &branch,
	}
	gitCommits, _, err := gitlabClient.Commits.ListCommits(fmt.Sprintf("%s/%s", impl.config.GitlabGroupPath, repoName), listCommitOptions)
	if err != nil {
		impl.logger.Errorw("error in getting commits", "err", err, "repoName", repoName)
		return nil, err
	}
	var gitCommitsDto []*GitCommitDto
	for _, gitCommit := range gitCommits {
		gitCommitDto := &GitCommitDto{
			CommitHash: gitCommit.String(),
			AuthorName: gitCommit.AuthorName,
			CommitTime: *gitCommit.AuthoredDate,
		}
		gitCommitsDto = append(gitCommitsDto, gitCommitDto)
	}
	return gitCommitsDto, nil
}
