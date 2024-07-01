package local

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/amplitude/analytics-go/amplitude"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"sync"

	"github.com/amplitude/experiment-go-server/internal/evaluation"

	"github.com/amplitude/experiment-go-server/pkg/experiment"

	"github.com/amplitude/experiment-go-server/internal/logger"
)

var clients = map[string]*Client{}
var initMutex = sync.Mutex{}

type Client struct {
	log               *logger.Log
	apiKey            string
	config            *Config
	client            *http.Client
	poller            *poller
	flagsMutex        *sync.RWMutex
	engine            *evaluation.Engine
	assignmentService *assignmentService
	cohortStorage     CohortStorage
	flagConfigStorage FlagConfigStorage
	cohortLoader      *CohortLoader
	deploymentRunner  *DeploymentRunner
}

func Initialize(apiKey string, config *Config) *Client {
	initMutex.Lock()
	client := clients[apiKey]
	if client == nil {
		if apiKey == "" {
			panic("api key must be set")
		}
		config = fillConfigDefaults(config)
		log := logger.New(config.Debug)
		var as *assignmentService
		if config.AssignmentConfig != nil && config.AssignmentConfig.APIKey != "" {
			amplitudeClient := amplitude.NewClient(config.AssignmentConfig.Config)
			as = &assignmentService{
				amplitude: &amplitudeClient,
				filter:    newAssignmentFilter(config.AssignmentConfig.CacheCapacity),
			}
		}
		cohortStorage := NewInMemoryCohortStorage()
		flagConfigStorage := NewInMemoryFlagConfigStorage()
		var cohortLoader *CohortLoader
		var deploymentRunner *DeploymentRunner
		if config.CohortSyncConfig != nil {
			cohortDownloadApi := NewDirectCohortDownloadApi(config.CohortSyncConfig.ApiKey, config.CohortSyncConfig.SecretKey, config.CohortSyncConfig.MaxCohortSize, config.CohortSyncConfig.CohortRequestDelayMillis, config.CohortSyncConfig.CohortServerUrl, config.Debug)
			cohortLoader = NewCohortLoader(cohortDownloadApi, cohortStorage)
		}
		deploymentRunner = NewDeploymentRunner(config, NewFlagConfigApiV2(apiKey, config.ServerUrl, config.FlagConfigPollerRequestTimeout), flagConfigStorage, cohortStorage, cohortLoader)
		client = &Client{
			log:               log,
			apiKey:            apiKey,
			config:            config,
			client:            &http.Client{},
			poller:            newPoller(),
			flagsMutex:        &sync.RWMutex{},
			engine:            evaluation.NewEngine(log),
			assignmentService: as,
			cohortStorage:     cohortStorage,
			flagConfigStorage: flagConfigStorage,
			cohortLoader:      cohortLoader,
			deploymentRunner:  deploymentRunner,
		}
		client.log.Debug("config: %v", *config)
		clients[apiKey] = client
	}
	initMutex.Unlock()
	return client
}

func (c *Client) Start() error {
	err := c.deploymentRunner.Start()
	if err != nil {
		return err
	}
	return nil
}

// Deprecated: Use EvaluateV2
func (c *Client) Evaluate(user *experiment.User, flagKeys []string) (map[string]experiment.Variant, error) {
	variants, err := c.EvaluateV2(user, flagKeys)
	if err != nil {
		return nil, err
	}
	results := make(map[string]experiment.Variant)
	for key, variant := range variants {
		isDefault, ok := variant.Metadata["default"].(bool)
		if !ok {
			isDefault = false
		}
		isDeployed, ok := variant.Metadata["deployed"].(bool)
		if !ok {
			isDeployed = true
		}
		if !isDefault && isDeployed {
			results[key] = variant
		}
	}
	return results, nil
}

func (c *Client) EvaluateV2(user *experiment.User, flagKeys []string) (map[string]experiment.Variant, error) {
	flagConfigs := c.flagConfigStorage.GetFlagConfigs()
	enrichedUser, err := c.enrichUser(user, flagConfigs)
	if err != nil {
		return nil, err
	}
	userContext := evaluation.UserToContext(enrichedUser)
	c.flagsMutex.RLock()
	sortedFlags, err := topologicalSort(flagConfigs, flagKeys)
	c.flagsMutex.RUnlock()
	if err != nil {
		return nil, err
	}
	c.log.Debug("evaluate:\n\t- user: %v\n\t- flags: %v\n", user, sortedFlags)
	results := c.engine.Evaluate(userContext, sortedFlags)
	variants := make(map[string]experiment.Variant)
	for key, result := range results {
		variants[key] = experiment.Variant{
			Key:      result.Key,
			Value:    coerceString(result.Value),
			Payload:  result.Payload,
			Metadata: result.Metadata,
		}
	}
	if c.assignmentService != nil {
		c.assignmentService.Track(newAssignment(user, variants))
	}
	return variants, nil
}

func (c *Client) FlagsV2() (string, error) {
	flags, err := c.doFlagsV2()
	if err != nil {
		return "", err
	}
	flagsJson, err := json.Marshal(flags)
	if err != nil {
		return "", err
	}
	flagsString := string(flagsJson)
	return flagsString, nil
}

// FlagMetadata returns a copy of the flag's metadata. If the flag is not found then nil is returned.
func (c *Client) FlagMetadata(flagKey string) map[string]interface{} {
	f := c.flagConfigStorage.GetFlagConfig(flagKey)
	if f == nil {
		return nil
	}

	metadata := make(map[string]interface{})
	for k, v := range f.Metadata {
		metadata[k] = v
	}

	return metadata
}

func (c *Client) doFlagsV2() (map[string]*evaluation.Flag, error) {
	client := &http.Client{}
	endpoint, err := url.Parse("https://api.lab.amplitude.com/")
	if err != nil {
		return nil, err
	}
	endpoint.Path = "sdk/v2/flags"
	endpoint.RawQuery = "v=0"
	ctx, cancel := context.WithTimeout(context.Background(), c.config.FlagConfigPollerRequestTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", fmt.Sprintf("Api-Key %s", c.apiKey))
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Amp-Exp-Library", fmt.Sprintf("experiment-go-server/%v", experiment.VERSION))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var flagsArray []*evaluation.Flag
	err = json.Unmarshal(body, &flagsArray)
	if err != nil {
		return nil, err
	}
	flags := make(map[string]*evaluation.Flag)
	for _, flag := range flagsArray {
		flags[flag.Key] = flag
	}
	return flags, nil
}

// Deprecated: This function returns an old data model that is no longer used.
func (c *Client) Rules() (map[string]interface{}, error) {
	return c.doRules()
}

func (c *Client) doRules() (map[string]interface{}, error) {
	endpoint, err := url.Parse(c.config.ServerUrl)
	if err != nil {
		return nil, err
	}
	endpoint.Path = "sdk/rules"
	endpoint.RawQuery = "eval_mode=local"
	ctx, cancel := context.WithTimeout(context.Background(), c.config.FlagConfigPollerRequestTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", fmt.Sprintf("Api-Key %s", c.apiKey))
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Amp-Exp-Library", fmt.Sprintf("experiment-go-server/%v", experiment.VERSION))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	c.log.Debug("rules: %v", string(body))
	var rules []map[string]interface{}
	err = json.Unmarshal(body, &rules)
	if err != nil {
		return nil, err
	}
	var result = make(map[string]interface{})
	for _, rule := range rules {
		flagKey := rule["flagKey"]
		result[fmt.Sprintf("%v", flagKey)] = rule
	}
	return result, nil
}

// Deprecated: This function returns an old data model that is no longer used.
func (c *Client) Flags() (*string, error) {
	flags, err := c.doFlags()
	if err != nil {
		return nil, err
	}
	flagsJson, err := json.Marshal(flags)
	if err != nil {
		return nil, err
	}
	flagsString := string(flagsJson)
	return &flagsString, nil
}

func (c *Client) doFlags() (map[string]interface{}, error) {
	endpoint, err := url.Parse(c.config.ServerUrl)
	if err != nil {
		return nil, err
	}
	endpoint.Path = "sdk/v1/flags"
	ctx, cancel := context.WithTimeout(context.Background(), c.config.FlagConfigPollerRequestTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", fmt.Sprintf("Api-Key %s", c.apiKey))
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Amp-Exp-Library", fmt.Sprintf("experiment-go-server/%v", experiment.VERSION))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	c.log.Debug("flags: %v", string(body))
	flagsArray := make([]interface{}, 0)
	err = json.Unmarshal(body, &flagsArray)
	if err != nil {
		return nil, err
	}
	// Extract keys and create flags map
	flags := make(map[string]interface{})
	for _, flagAny := range flagsArray {
		switch flag := flagAny.(type) {
		case map[string]interface{}:
			switch flagKey := flag["flagKey"].(type) {
			case string:
				flags[flagKey] = flag
			}
		}
	}
	return flags, nil
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func coerceString(value interface{}) string {
	if value == nil {
		return ""
	}
	kind := reflect.TypeOf(value).Kind()
	if kind == reflect.Map || kind == reflect.Slice || kind == reflect.Array {
		b, err := json.Marshal(value)
		if err == nil {
			return string(b)
		}
	}
	return fmt.Sprintf("%v", value)
}

func (c *Client) enrichUser(user *experiment.User, flagConfigs map[string]*evaluation.Flag) (*experiment.User, error) {
	flagConfigSlice := make([]*evaluation.Flag, 0, len(flagConfigs))

	for _, value := range flagConfigs {
		flagConfigSlice = append(flagConfigSlice, value)
	}
	groupedCohortIDs := getGroupedCohortIDsFromFlags(flagConfigSlice)

	if cohortIDs, ok := groupedCohortIDs[userGroupType]; ok {
		if len(cohortIDs) > 0 && user.UserId != "" {
			user.CohortIds = c.cohortStorage.GetCohortsForUser(user.UserId, cohortIDs)
		}
	}

	if user.Groups != nil {
		for groupType, groupNames := range user.Groups {
			groupName := ""
			if len(groupNames) > 0 {
				groupName = groupNames[0]
			}
			if groupName == "" {
				continue
			}
			if cohortIDs, ok := groupedCohortIDs[groupType]; ok {
				user.AddGroupCohortIDs(groupType, groupName, c.cohortStorage.GetCohortsForGroup(groupType, groupName, cohortIDs))
			}
		}
	}
	return user, nil
}
