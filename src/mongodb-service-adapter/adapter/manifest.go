package adapter

import (
	"fmt"
	"log"
	"strings"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
)

const (
	StemcellAlias           = "mongodb-stemcell"
	MongodInstanceGroupName = "mongod_node"
	MongodJobName           = "mongod_node"
	AliasesJobName          = "mongodb-dns-aliases"
	BoshDNSEnableJobName    = "bosh-dns-enable"
	ConfigAgentJobName      = "mongodb_config_agent"
	CleanupErrandJobName    = "cleanup_service"
	LifecycleErrandType     = "errand"
)

type ManifestGenerator struct {
	Logger *log.Logger
}

func (m *ManifestGenerator) logf(msg string, v ...interface{}) {
	if m.Logger != nil {
		m.Logger.Printf(msg, v...)
	}
}

func (m ManifestGenerator) GenerateManifest(
	serviceDeployment serviceadapter.ServiceDeployment,
	plan serviceadapter.Plan,
	requestParams serviceadapter.RequestParameters,
	previousManifest *bosh.BoshManifest,
	previousPlan *serviceadapter.Plan) (bosh.BoshManifest, error) {

	m.logf("request params: %#v", requestParams)

	arbitraryParams := requestParams.ArbitraryParams()

	mongoOps := plan.Properties["mongo_ops"].(map[string]interface{})

	username := mongoOps["username"].(string)
	apiKey := mongoOps["api_key"].(string)
	boshDNSDisable := mongoOps["bosh_dns_disable"].(bool)

	// trim trailing slash
	url := mongoOps["url"].(string)
	url = strings.TrimRight(url, "/")

	oc := &OMClient{Url: url, Username: username, ApiKey: apiKey}

	var previousMongoProperties map[interface{}]interface{}

	if previousManifest != nil {
		previousMongoProperties = mongoPlanProperties(*previousManifest)
	}

	adminPassword, err := passwordForMongoServer(previousMongoProperties)
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	id, err := idForMongoServer(previousMongoProperties)
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	group, err := groupForMongoServer(id, oc, plan.Properties, previousMongoProperties, arbitraryParams)
	if err != nil {
		return bosh.BoshManifest{}, fmt.Errorf("could not create new group (%s)", err.Error())
	}
	m.logf("created group %s", group.ID)

	releases := []bosh.Release{}
	for _, release := range serviceDeployment.Releases {
		releases = append(releases, bosh.Release{
			Name:    release.Name,
			Version: release.Version,
		})
	}

	mongodInstanceGroup := findInstanceGroup(plan, MongodInstanceGroupName)
	if mongodInstanceGroup == nil {
		return bosh.BoshManifest{}, fmt.Errorf("no definition found for instance group '%s'", MongodInstanceGroupName)
	}

	mongodJobs, err := gatherJobs(serviceDeployment.Releases, []string{MongodJobName})
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	configAgentJobs, err := gatherJobs(serviceDeployment.Releases, []string{ConfigAgentJobName})
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	cleanupErrandJobs, err := gatherJobs(serviceDeployment.Releases, []string{CleanupErrandJobName})
	if err != nil {
		return bosh.BoshManifest{}, err
	}

	addonsJobs, err := gatherJobs(serviceDeployment.Releases, []string{AliasesJobName, BoshDNSEnableJobName})
	if err != nil {
		return bosh.BoshManifest{}, err
	}
	if boshDNSDisable {
		addonsJobs, err = gatherJobs(serviceDeployment.Releases, []string{AliasesJobName})
		if err != nil {
			return bosh.BoshManifest{}, err
		}
	}

	mongodNetworks := []bosh.Network{}
	for _, network := range mongodInstanceGroup.Networks {
		mongodNetworks = append(mongodNetworks, bosh.Network{Name: network})
	}
	if len(mongodNetworks) == 0 {
		return bosh.BoshManifest{}, fmt.Errorf("no networks definition found for instance group '%s'", MongodInstanceGroupName)
	}

	var engineVersion string
	version := getArbitraryParam("version", "engine_version", arbitraryParams, previousMongoProperties)
	if version == nil {
		engineVersion = oc.GetLatestVersion(group.ID)
	} else {
		engineVersion = version.(string)
	}

	// sharded_cluster parameters
	replicas := 0
	shards := 0
	routers := 0
	configServers := 0

	// total number of instances
	//
	// standalone:      always one
	// replica_set:     number of replicas
	// sharded_cluster: shards*replicas + config_servers + mongos
	instances := mongodInstanceGroup.Instances

	planID := plan.Properties["id"].(string)
	switch planID {
	case PlanStandalone:
		// ok
	case PlanReplicaSet:
		r := getArbitraryParam("replicas", "replicas", arbitraryParams, previousMongoProperties)
		if r != nil {
			instances = r.(int)
		}
		replicas = instances
	case PlanShardedCluster:
		shards = 2
		s := getArbitraryParam("shards", "shards", arbitraryParams, previousMongoProperties)
		if s != nil {
			shards = s.(int)
		}

		replicas = 3
		r := getArbitraryParam("replicas", "replicas", arbitraryParams, previousMongoProperties)
		if r != nil {
			replicas = r.(int)
		}

		configServers = 3
		c := getArbitraryParam("config_servers", "config_servers", arbitraryParams, previousMongoProperties)
		if c != nil {
			configServers = c.(int)
		}

		routers = 2
		r = getArbitraryParam("mongos", "routers", arbitraryParams, previousMongoProperties)
		if r != nil {
			routers = r.(int)
		}

		instances = routers + configServers + shards*replicas
	default:
		return bosh.BoshManifest{}, fmt.Errorf("unknown plan: %s", planID)
	}
	authKey, err := authKeyForMongoServer(previousMongoProperties)
	if err != nil {
		return bosh.BoshManifest{}, err
	}
	backupEnabled := false
	if planID != PlanStandalone {
		e := getArbitraryParam("backup_enabled", "backup_enabled", arbitraryParams, previousMongoProperties)
		if e != nil {
			backupEnabled = e.(bool)
		} else {
			backupEnabled = mongoOps["backup_enabled"].(bool)
		}
	}
	tlsEnabled := false
	e := getArbitraryParam("ssl_enabled", "ssl_enabled", arbitraryParams, previousMongoProperties)
	if e != nil {
		tlsEnabled = e.(bool)
	}

	caCert := ""
	if mongoOps["ssl_ca_cert"] != "" {
		caCert = mongoOps["ssl_ca_cert"].(string)
	}
	sslPem := ""
	if mongoOps["ssl_pem"] != "" {
		sslPem = mongoOps["ssl_pem"].(string)
	}

	updateBlock := &bosh.Update{
		Canaries:        1,
		CanaryWatchTime: "3000-180000",
		UpdateWatchTime: "3000-180000",
		MaxInFlight:     4,
	}

	manifest := bosh.BoshManifest{
		Name:     serviceDeployment.DeploymentName,
		Releases: releases,
		Addons: []bosh.Addon{
			bosh.Addon{
				Name: "mongodb-dns-helpers",
				Jobs: addonsJobs,
			},
		},
		Stemcells: []bosh.Stemcell{
			{
				Alias:   StemcellAlias,
				OS:      serviceDeployment.Stemcell.OS,
				Version: serviceDeployment.Stemcell.Version,
			},
		},
		InstanceGroups: []bosh.InstanceGroup{
			{
				Name:               MongodInstanceGroupName,
				Instances:          instances,
				Jobs:               mongodJobs,
				VMType:             mongodInstanceGroup.VMType,
				VMExtensions:       mongodInstanceGroup.VMExtensions,
				Stemcell:           StemcellAlias,
				PersistentDiskType: mongodInstanceGroup.PersistentDiskType,
				AZs:                mongodInstanceGroup.AZs,
				Networks:           mongodNetworks,
				Properties:         map[string]interface{}{},
			},
			{
				Name:         "mongodb-config-agent",
				Instances:    1,
				Jobs:         configAgentJobs,
				VMType:       mongodInstanceGroup.VMType,
				VMExtensions: mongodInstanceGroup.VMExtensions,
				Stemcell:     StemcellAlias,
				AZs:          mongodInstanceGroup.AZs,
				Networks:     mongodNetworks,

				// See mongodb_config_agent job spec
				Properties: map[string]interface{}{
					"mongo_ops": map[string]interface{}{
						"id":               id,
						"url":              url,
						"agent_api_key":    group.AgentAPIKey,
						"api_key":          apiKey,
						"auth_key":         authKey,
						"username":         username,
						"group_id":         group.ID,
						"plan_id":          planID,
						"admin_password":   adminPassword,
						"engine_version":   engineVersion,
						"routers":          routers,
						"config_servers":   configServers,
						"replicas":         replicas,
						"shards":           shards,
						"backup_enabled":   backupEnabled,
						"ssl_enabled":      tlsEnabled,
						"ssl_ca_cert":      caCert,
						"ssl_pem":          sslPem,
						"bosh_dns_disable": boshDNSDisable,
					},
				},
			},
			{
				Name:         "cleanup-service",
				Instances:    1,
				Jobs:         cleanupErrandJobs,
				VMType:       mongodInstanceGroup.VMType,
				VMExtensions: mongodInstanceGroup.VMExtensions,
				Stemcell:     StemcellAlias,
				AZs:          mongodInstanceGroup.AZs,
				Networks:     mongodNetworks,
				Lifecycle:    LifecycleErrandType,
				Properties:   map[string]interface{}{},
			},
		},
		Update: updateBlock,
		Properties: map[string]interface{}{
			"mongo_ops": map[string]interface{}{
				"url":            url,
				"api_key":        group.AgentAPIKey,
				"group_id":       group.ID,
				"admin_password": adminPassword,
				"ssl_enabled":    tlsEnabled,

				// options needed for binding
				"plan_id":        planID,
				"routers":        routers,
				"config_servers": configServers,
				"replicas":       replicas,
			},
		},
	}

	m.logf("generated manifest: %#v", manifest)
	return manifest, nil
}

func findInstanceGroup(plan serviceadapter.Plan, jobName string) *serviceadapter.InstanceGroup {
	for _, instanceGroup := range plan.InstanceGroups {
		if instanceGroup.Name == jobName {
			return &instanceGroup
		}
	}
	return nil
}

func gatherJobs(releases serviceadapter.ServiceReleases, requiredJobs []string) ([]bosh.Job, error) {
	jobs := []bosh.Job{}
	for _, requiredJob := range requiredJobs {
		release, err := findReleaseForJob(releases, requiredJob)
		if err != nil {
			return nil, err
		}

		job := bosh.Job{
			Name:     requiredJob,
			Release:  release.Name,
			Consumes: map[string]interface{}{},
			Provides: map[string]bosh.ProvidesLink{},
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

func mongoPlanProperties(manifest bosh.BoshManifest) map[interface{}]interface{} {
	return manifest.InstanceGroups[1].Properties["mongo_ops"].(map[interface{}]interface{})
}

func passwordForMongoServer(previousManifestProperties map[interface{}]interface{}) (string, error) {
	if previousManifestProperties != nil {
		return previousManifestProperties["admin_password"].(string), nil
	}

	return GenerateString(20)
}

func idForMongoServer(previousManifestProperties map[interface{}]interface{}) (string, error) {
	if previousManifestProperties != nil {
		return previousManifestProperties["id"].(string), nil
	}

	return GenerateString(8)
}

func authKeyForMongoServer(previousManifestProperties map[interface{}]interface{}) (string, error) {
	if previousManifestProperties != nil {
		return previousManifestProperties["auth_key"].(string), nil
	}

	return GenerateString(8)
}

func groupForMongoServer(mongoID string, oc *OMClient,
	planProperties map[string]interface{},
	previousMongoProperties map[interface{}]interface{},
	arbitraryParams map[string]interface{}) (Group, error) {

	req := GroupCreateRequest{}
	if name, found := arbitraryParams["projectName"]; found {
		req.Name = strings.ToUpper(name.(string))
	}
	if orgId, found := arbitraryParams["orgId"]; found {
		req.OrgId = strings.ToUpper(orgId.(string))
	}
	tags := planProperties["mongo_ops"].(map[string]interface{})["tags"]
	if tags != nil {
		t := tags.([]interface{})
		for _, tag := range t {
			req.Tags = append(req.Tags, tag.(map[string]interface{})["tag_name"].(string))
		}
	}

	if previousMongoProperties != nil {
		group, err := oc.UpdateGroup(previousMongoProperties["group_id"].(string), GroupUpdateRequest{req.Tags})
		if err != nil {
			return Group{}, err
		}
		// AgentAPIKey is empty for PATCH and GET requests in OM 3.6, taking the value from previous manifest instead
		group.AgentAPIKey = previousMongoProperties["agent_api_key"].(string)
		return group, nil
	} else {
		return oc.CreateGroup(mongoID, req)
	}
}

func findReleaseForJob(releases serviceadapter.ServiceReleases, requiredJob string) (serviceadapter.ServiceRelease, error) {
	releasesThatProvideRequiredJob := serviceadapter.ServiceReleases{}

	for _, release := range releases {
		for _, providedJob := range release.Jobs {
			if providedJob == requiredJob {
				releasesThatProvideRequiredJob = append(releasesThatProvideRequiredJob, release)
			}
		}
	}

	if len(releasesThatProvideRequiredJob) == 0 {
		return serviceadapter.ServiceRelease{}, fmt.Errorf("no release provided for job '%s'", requiredJob)
	}

	if len(releasesThatProvideRequiredJob) > 1 {
		releaseNames := []string{}
		for _, release := range releasesThatProvideRequiredJob {
			releaseNames = append(releaseNames, release.Name)
		}

		return serviceadapter.ServiceRelease{}, fmt.Errorf("job '%s' defined in multiple releases: %s", requiredJob, strings.Join(releaseNames, ", "))
	}

	return releasesThatProvideRequiredJob[0], nil
}

func getArbitraryParam(propName string, manifestName string, arbitraryParams map[string]interface{}, previousMongoProperties map[interface{}]interface{}) interface{} {
	var prop interface{}
	var found bool
	if prop, found = arbitraryParams[propName]; found {
		goto found
	}
	if prop, found = previousMongoProperties[manifestName]; found {
		goto found
	}
	return nil
found:
	// we are interested only in string, bool and int properties, though json conversion returns float64 for all integer properties
	if p, ok := prop.(float64); ok {
		return int(p)
	}
	return prop
}
