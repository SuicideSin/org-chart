package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"

	"github.com/sirupsen/logrus"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"

	"github.com/lancecarlson/couchgo"

	"gopkg.in/urfave/cli.v1"
)

func main() {

	app := cli.NewApp()

	app.Name = "org-chart management"

	app.Commands = []cli.Command{
		{
			Name: "gh-sync",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name: "data-url",
				},
				cli.StringFlag{
					Name:   "github-token",
					EnvVar: "GITHUB_TOKEN",
				},
				cli.StringFlag{
					Name: "github-org",
				},
				cli.StringFlag{
					Name:  "github-team-prefix",
					Value: "org-",
				},
				cli.BoolFlag{
					Name: "dry-run",
				},
			},
			Action: func(c *cli.Context) error {

				//logrus.SetLevel(logrus.DebugLevel)

				orgChart, err := loadOrgChartData(c.String("data-url"))

				if err != nil {
					return errors.Wrap(err, "retrieving org chart data")
				}

				for _, t := range orgChart.Teams {
					t.Github = fmt.Sprintf("%s%s", c.String("github-team-prefix"), strings.Replace(t.ID, "_", "-", -1))
				}

				gh, err := newGithubState(c.String("github-token"), c.String("github-org"), c.String("github-team-prefix"))

				if err != nil {
					return errors.Wrap(err, "retrieving github data")
				}

				gh.dry = c.Bool("dry-run")

				if gh.dry {
					logrus.Info("running in DRY mode")
				}

				for _, m := range githubMembersNotInOrgchart(orgChart, gh) {
					logrus.Infof("github user %s not found in orgchart", m.GetLogin())
				}

				for _, m := range employeesNotInGithub(orgChart, gh) {
					logrus.Infof("employee %s (%s) not found in github, will be added", m.Name, m.Github)
				}

				for _, t := range githubTeamsNotInOrgchart(orgChart, gh) {
					logrus.Infof("github team %s not found in orgchart, will be removed", t.GetName())
				}

				for _, m := range teamsNotInGithub(orgChart, gh) {
					logrus.Infof("team %s (%s) not found in github, will be added", m.Name, m.Github)
				}

				result, err := gh.SyncTeams(orgChart)

				if err != nil {
					return errors.Wrap(err, "syncing teams")
				}

				for _, team := range result.removedTeams {
					logrus.Infof("removed %s from github", team.GetName())
				}

				for _, employee := range result.unableToCreateMembership {
					logrus.Infof("unable to add member %s to %s team, github handle not provided", employee.Name, employee.MemberOf)
				}

				for _, employee := range result.unableToCreateMaintainer {
					logrus.Infof("unable to add maintainer %s to %s team, github handle not provided", employee.Name, employee.MemberOf)
				}

				return nil

			},
		},
	}

	err := app.Run(os.Args)

	if err != nil {
		logrus.Fatal(err)
	}

}

type Employee struct {
	ID       string
	Name     string
	Github   string
	MemberOf string
	Team     *Team
}

type Team struct {
	ID            string
	Name          string
	ParentID      string `json:"parent"`
	Description   string
	Github        string
	TeachLeadID   string `json:"techLead"`
	ProductLeadID string `json:"productLead"`
}

type OrgChart struct {
	Employees     []*Employee
	Teams         []*Team
	TeamsByID     map[string]*Team
	EmployeesByID map[string]*Employee
}

func (oc *OrgChart) organise() error {

	oc.TeamsByID = make(map[string]*Team)
	oc.EmployeesByID = make(map[string]*Employee)

	for _, t := range oc.Teams {
		oc.TeamsByID[t.ID] = t
	}

	for _, e := range oc.Employees {

		oc.EmployeesByID[e.ID] = e

		team, ok := oc.TeamsByID[e.MemberOf]

		if !ok {
			return errors.Errorf("could not find team %s for member %s", e.MemberOf, e.Name)
		}

		e.Team = team
	}

	return nil

}

func employeesNotInGithub(orgchart *OrgChart, gh *GithubState) []*Employee {

	notInGithub := []*Employee{}

	for _, employee := range orgchart.Employees {
		if employee.Github == "" {
			continue
		}

		found := false

		for _, ghMember := range gh.members {
			if employee.Github == ghMember.GetLogin() {
				found = true
				break
			}
		}

		if !found {
			notInGithub = append(notInGithub, employee)
		}
	}

	return notInGithub
}

func githubMembersNotInOrgchart(orgchart *OrgChart, gh *GithubState) []*github.User {
	notInOrgchart := []*github.User{}

	for _, ghMember := range gh.members {
		found := false
		for _, employee := range orgchart.Employees {
			if employee.Github == ghMember.GetLogin() {
				found = true
				break
			}
		}
		if !found {
			notInOrgchart = append(notInOrgchart, ghMember)
		}
	}

	return notInOrgchart
}

func teamsNotInGithub(orgchart *OrgChart, gh *GithubState) []*Team {

	notInGithub := []*Team{}

	for _, team := range orgchart.Teams {

		found := false

		for _, ghTeam := range gh.teams {
			if team.Github == ghTeam.GetName() {
				found = true
				break
			}
		}

		if !found {
			notInGithub = append(notInGithub, team)
		}
	}

	return notInGithub

}

func githubTeamsNotInOrgchart(orgchart *OrgChart, gh *GithubState) []*github.Team {
	notInOrgchart := []*github.Team{}

	for _, ghTeam := range gh.teams {
		found := false
		for _, team := range orgchart.Teams {
			if team.Github == ghTeam.GetName() {
				found = true
				break
			}
		}
		if !found {
			notInOrgchart = append(notInOrgchart, ghTeam)
		}
	}

	return notInOrgchart
}

func newGithubState(token, organisation, teamPrefix string) (*GithubState, error) {

	client := newGitHubClient(token)

	gh := &GithubState{
		organisation: organisation,
		teamPrefix:   teamPrefix,
		client:       client,
		teams:        make(map[string]*github.Team),
		members:      []*github.User{},
	}

	ctx := context.Background()

	memberOpt := &github.ListMembersOptions{
		ListOptions: github.ListOptions{PerPage: 500},
	}

	for {
		members, res, err := client.Organizations.ListMembers(ctx, organisation, memberOpt)

		if err != nil {
			return nil, err
		}

		gh.AddMembers(members...)

		if res.NextPage == 0 {
			break
		}

		memberOpt.Page = res.NextPage
	}

	teamsOpt := &github.ListOptions{PerPage: 500}

	for {
		teams, res, err := client.Teams.ListTeams(ctx, organisation, teamsOpt)

		if err != nil {
			return nil, err
		}

		for _, t := range teams {
			if strings.HasPrefix(t.GetName(), teamPrefix) {
				gh.AddTeam(t)
			}
		}

		if res.NextPage == 0 {
			break
		}

		teamsOpt.Page = res.NextPage
	}

	return gh, nil
}

type githubSyncResult struct {
	removedTeams             []*github.Team
	unableToCreateMembership []*Employee
	unableToCreateMaintainer []*Employee
}

type GithubState struct {
	organisation string
	teamPrefix   string
	client       *github.Client
	teams        map[string]*github.Team
	members      []*github.User
	syncResult   *githubSyncResult
	dry          bool
	orgTeams     []*Team
}

func (gh *GithubState) AddTeam(team *github.Team) {
	gh.teams[team.GetName()] = team
}

func (gh *GithubState) AddMembers(member ...*github.User) {
	gh.members = append(gh.members, member...)
}

func (gh *GithubState) createTeamByIDIfNotExists(teamID string) (*github.Team, error) {

	var teamToCreate *Team

	for _, team := range gh.orgTeams {
		if team.ID == teamID {
			teamToCreate = team
		}
	}

	if teamToCreate == nil {
		return nil, errors.Errorf("could not find org team %s for creation", teamID)
	}

	var parentID *int64

	if teamToCreate.ParentID != "" {
		parentTeam, err := gh.createTeamByIDIfNotExists(teamToCreate.ParentID)
		if err != nil {
			return nil, err
		}
		parentID = parentTeam.ID
	}

	privacy := "closed"

	ctx := context.Background()

	if preExistingTeam, ok := gh.teams[teamToCreate.Github]; ok {
		if parentTeam := preExistingTeam.GetParent(); parentTeam != nil {
			if parentTeam.GetName() != teamToCreate.ParentID {

				if !gh.dry {

					editedTeam, _, err := gh.client.Teams.EditTeam(ctx, preExistingTeam.GetID(), github.NewTeam{
						Name:         teamToCreate.Github,
						Description:  &teamToCreate.Description,
						ParentTeamID: parentID,
						Privacy:      &privacy,
					})

					if err != nil {
						return nil, errors.Wrap(err, "editing team")
					}

					gh.teams[editedTeam.GetName()] = editedTeam
				}

			}
		}
		return preExistingTeam, nil
	}

	var createdTeam *github.Team

	if gh.dry {
		createdTeam = &github.Team{
			Name:        &teamToCreate.Github,
			Description: &teamToCreate.Description,
			Privacy:     &privacy,
		}
	} else {

		var err error

		createdTeam, _, err = gh.client.Teams.CreateTeam(ctx, gh.organisation, github.NewTeam{
			Name:         teamToCreate.Github,
			Description:  &teamToCreate.Description,
			ParentTeamID: parentID,
			Privacy:      &privacy,
		})

		if err != nil {
			return nil, err
		}
	}

	gh.teams[createdTeam.GetName()] = createdTeam

	return createdTeam, nil

}

func (gh *GithubState) removeTeam(team *github.Team) error {

	if !gh.dry {

		_, err := gh.client.Teams.DeleteTeam(context.Background(), team.GetID())

		if err != nil {
			return nil
		}

	}

	gh.syncResult.removedTeams = append(gh.syncResult.removedTeams, team)

	delete(gh.teams, team.GetName())

	return nil
}

type teamMembershipSync struct {
	Maintainers []string
	Members     []string
}

func teamMembersSyncData(chart *OrgChart, gh *GithubState) (map[*github.Team]*teamMembershipSync, error) {

	memberSync := map[*github.Team]*teamMembershipSync{}

	for _, e := range chart.Employees {

		if e.Github == "" {
			gh.syncResult.unableToCreateMembership = append(gh.syncResult.unableToCreateMembership, e)
			continue
		}

		team, ok := gh.teams[e.Team.Github]

		if !ok {
			return nil, errors.Errorf("team %s not found in github", e.Team.Github)
		}

		if _, ok := memberSync[team]; !ok {
			memberSync[team] = &teamMembershipSync{
				Maintainers: []string{},
				Members:     []string{},
			}
		}

		memberSync[team].Members = append(memberSync[team].Members, e.Github)
	}

	for _, t := range chart.Teams {

		team, ok := gh.teams[t.Github]

		if !ok {
			return nil, errors.Errorf("team %s not found in github", t.Github)
		}

		if _, ok := memberSync[team]; !ok {
			memberSync[team] = &teamMembershipSync{
				Maintainers: []string{},
				Members:     []string{},
			}
		}

		if t.TeachLeadID != "" {
			techLead, ok := chart.EmployeesByID[t.TeachLeadID]

			if !ok {
				return nil, errors.Errorf("could not find tech lead %s for team %s", t.TeachLeadID, t.Name)
			}

			if techLead.Github == "" {
				gh.syncResult.unableToCreateMaintainer = append(gh.syncResult.unableToCreateMaintainer, techLead)
				continue
			}

			memberSync[team].Maintainers = append(memberSync[team].Maintainers, techLead.Github)
		}

		if t.ProductLeadID != "" {
			productLead, ok := chart.EmployeesByID[t.ProductLeadID]

			if !ok {
				return nil, errors.Errorf("could not find product lead %s for team %s", t.TeachLeadID, t.Name)
			}

			if productLead.Github == "" {
				gh.syncResult.unableToCreateMaintainer = append(gh.syncResult.unableToCreateMaintainer, productLead)
				continue
			}

			memberSync[team].Maintainers = append(memberSync[team].Maintainers, productLead.Github)
		}
	}

	return memberSync, nil

}

func (gh *GithubState) SyncTeams(chart *OrgChart) (*githubSyncResult, error) {

	gh.orgTeams = chart.Teams

	gh.syncResult = &githubSyncResult{
		[]*github.Team{},
		[]*Employee{},
		[]*Employee{},
	}

	for _, teamToRemove := range githubTeamsNotInOrgchart(chart, gh) {
		err := gh.removeTeam(teamToRemove)

		if err != nil {
			return gh.syncResult, err
		}
	}

	for _, t := range chart.Teams {
		if t.ParentID == "" {
			// root team

			rootTeam, ok := gh.teams[t.Github]

			if !ok {
				break
			}

			err := gh.removeTeam(rootTeam)

			if err != nil {
				return gh.syncResult, err
			}

			gh.teams = map[string]*github.Team{}

		}
	}

	for _, teamToCreate := range teamsNotInGithub(chart, gh) {
		_, err := gh.createTeamByIDIfNotExists(teamToCreate.ID)

		if err != nil {
			return gh.syncResult, err
		}
	}

	syncData, err := teamMembersSyncData(chart, gh)

	if err != nil {
		return gh.syncResult, err
	}

	for ghTeam, membership := range syncData {
		err := gh.syncTeamMembers(ghTeam, membership.Members, membership.Maintainers)

		if err != nil {
			return gh.syncResult, err
		}
	}

	return gh.syncResult, nil

}

func (gh *GithubState) syncTeamMembers(team *github.Team, memberHandles []string, maintainerHandles []string) error {

	logrus.Infof("syncing members and maintainers for %s", team.GetName())

	if gh.dry {
		return nil
	}

	ctx := context.Background()

	for _, user := range memberHandles {

		logrus.Debugf("adding %s as member to %s", user, team.GetName())

		_, _, err := gh.client.Teams.AddTeamMembership(ctx, team.GetID(), user, nil)

		if err != nil {
			return err
		}

	}

	for _, user := range maintainerHandles {

		logrus.Debugf("adding %s as maintainer to %s", user, team.GetName())

		_, _, err := gh.client.Teams.AddTeamMembership(ctx, team.GetID(), user, &github.TeamAddTeamMembershipOptions{Role: "maintainer"})

		if err != nil {
			return err
		}
	}
	return nil
}

func newGitHubClient(token string) *github.Client {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)

	ctx := context.Background()

	tc := oauth2.NewClient(ctx, ts)

	return github.NewClient(tc)
}

func loadOrgChartData(location string) (*OrgChart, error) {

	URL, err := url.Parse(location)

	if err != nil {
		return nil, err
	}

	var chart OrgChart

	couchdb := couch.NewClient(URL)

	err = couchdb.Get("chart", &chart)

	if err != nil {
		return nil, err
	}

	err = chart.organise()

	if err != nil {
		return nil, err
	}

	return &chart, nil

}
