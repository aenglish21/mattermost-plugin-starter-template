package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
)

// Configuration defines the plugin configuration.
type Configuration struct {
	// Microsoft Graph settings.
	TenantID      string `json:"tenant_id"`
	GraphClientID string `json:"graph_client_id"`
	GraphSecret   string `json:"graph_secret"`

	// Mapping from Azure AD group display names to Mattermost team IDs.
	// Example: {"Developers": "TEAM_ID_FOR_DEVELOPERS", "Sales": "TEAM_ID_FOR_SALES"}
	Mapping map[string]string `json:"mapping"`
}

var configurationLock sync.RWMutex
var configuration *Configuration

// getConfiguration retrieves the active configuration under lock.
func (p *Plugin) getConfiguration() *Configuration {
	configurationLock.RLock()
	defer configurationLock.RUnlock()

	if configuration == nil {
		return &Configuration{}
	}
	// Make a shallow copy so that we don't modify the active configuration.
	cfgCopy := *configuration
	return &cfgCopy
}

// setConfiguration saves the active configuration under lock.
func (p *Plugin) setConfiguration(cfg *Configuration) {
	configurationLock.Lock()
	defer configurationLock.Unlock()
	configuration = cfg
}

// Plugin implements the interface expected by Mattermost.
type Plugin struct {
	plugin.MattermostPlugin
}

// OnConfigurationChange is invoked when configuration changes occur.
func (p *Plugin) OnConfigurationChange() error {
	var cfg Configuration

	if err := p.API.LoadPluginConfiguration(&cfg); err != nil {
		return err
	}

	p.setConfiguration(&cfg)
	p.API.LogInfo("Configuration updated", "config", fmt.Sprintf("%+v", cfg))
	return nil
}

// getGraphToken obtains an access token from Microsoft Graph using client credentials.
func (p *Plugin) getGraphToken() (string, error) {
	cfg := p.getConfiguration()
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", cfg.TenantID)
	data := fmt.Sprintf("client_id=%s&scope=https://graph.microsoft.com/.default&client_secret=%s&grant_type=client_credentials",
		cfg.GraphClientID, cfg.GraphSecret)
	req, err := http.NewRequest("POST", tokenURL, bytes.NewBufferString(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to get token: %s", body)
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	err = json.Unmarshal(body, &tokenResp)
	if err != nil {
		return "", err
	}
	return tokenResp.AccessToken, nil
}

// queryUserIDByEmail looks up the Azure AD user ID by email.
func (p *Plugin) queryUserIDByEmail(email, token string) (string, error) {
	url := fmt.Sprintf("https://graph.microsoft.com/v1.0/users?$filter=userPrincipalName eq '%s'", email)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to query user: %s", body)
	}
	var result struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	err = json.Unmarshal(body, &result)
	if err != nil {
		return "", err
	}
	if len(result.Value) == 0 {
		return "", fmt.Errorf("no user found for email %s", email)
	}
	return result.Value[0].ID, nil
}

// queryUserGroups retrieves the groups for a given Azure AD user ID.
func (p *Plugin) queryUserGroups(userID, token string) ([]string, error) {
	url := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/memberOf", userID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to query groups: %s", body)
	}
	var result struct {
		Value []struct {
			DisplayName string `json:"displayName"`
			ODataType   string `json:"@odata.type"`
		} `json:"value"`
	}
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}
	var groups []string
	for _, group := range result.Value {
		if group.ODataType == "#microsoft.graph.group" {
			groups = append(groups, group.DisplayName)
		}
	}
	return groups, nil
}

// syncUserGroups queries Azure AD for the user's groups and adds them to Mattermost teams based on the mapping.
func (p *Plugin) syncUserGroups(user *model.User) error {
	cfg := p.getConfiguration()
	email := user.Email
	token, err := p.getGraphToken()
	if err != nil {
		return err
	}
	azureUserID, err := p.queryUserIDByEmail(email, token)
	if err != nil {
		return err
	}
	groups, err := p.queryUserGroups(azureUserID, token)
	if err != nil {
		return err
	}
	p.API.LogInfo("Graph query returned groups", "email", email, "groups", groups)

	// For each mapping, if the user's Azure AD groups include the mapped group,
	// add the user to the corresponding Mattermost team.
	for azureGroup, mattermostTeamID := range cfg.Mapping {
		for _, group := range groups {
			if group == azureGroup {
				// Check if user is already a team member.
				if _, err := p.API.GetTeamMember(mattermostTeamID, user.Id); err != nil {
					p.API.LogInfo("Adding user to team", "user_id", user.Id, "team_id", mattermostTeamID)
					if err := p.API.AddTeamMember(mattermostTeamID, user.Id); err != nil {
						p.API.LogError("Failed to add user to team", "user_id", user.Id, "team_id", mattermostTeamID, "error", err.Error())
					}
				}
			}
		}
	}
	return nil
}

// OnUserCreated is triggered when a new user is created in Mattermost.
func (p *Plugin) OnUserCreated(c *plugin.Context, user *model.User) {
	p.API.LogInfo("OnUserCreated: syncing groups for user", "user_email", user.Email)
	if err := p.syncUserGroups(user); err != nil {
		p.API.LogError("Error syncing user groups on creation", "error", err.Error())
	}
}

// OnUserLogin is triggered when a user logs in.
func (p *Plugin) OnUserLogin(c *plugin.Context, user *model.User) {
	p.API.LogInfo("OnUserLogin: syncing groups for user", "user_email", user.Email)
	if err := p.syncUserGroups(user); err != nil {
		p.API.LogError("Error syncing user groups on login", "error", err.Error())
	}
}

func main() {
	plugin.ClientMain(&Plugin{})
}
