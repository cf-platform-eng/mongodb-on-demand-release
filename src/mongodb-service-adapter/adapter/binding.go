package adapter

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
	"gopkg.in/mgo.v2"
)

type Binder struct {
	Logger *log.Logger
}

func (b *Binder) logf(msg string, v ...interface{}) {
	if b.Logger != nil {
		b.Logger.Printf(msg, v...)
	}
}

const (
	adminDB   = "admin"
	defaultDB = "default"
)

func (b Binder) CreateBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) (serviceadapter.Binding, error) {

	// create an admin level user
	username := mkUsername(bindingID)
	password, err := GenerateString(32)
	if err != nil {
		return serviceadapter.Binding{}, err
	}

	properties := manifest.Properties["mongo_ops"].(map[interface{}]interface{})
	adminPassword := properties["admin_password"].(string)

	b.logf("properties: %v", properties)

	servers := make([]string, len(deploymentTopology["mongod_node"]))
	for i, node := range deploymentTopology["mongod_node"] {
		servers[i] = fmt.Sprintf("%s:28000", node)
	}

	plan := properties["plan_id"].(string)
	if plan == PlanShardedCluster {
		routers := properties["routers"].(int)
		configServers := properties["config_servers"].(int)
		replicas := properties["replicas"].(int)

		cluster, err := NodesToCluster(servers, routers, configServers, replicas)
		if err != nil {
			return serviceadapter.Binding{}, err
		}
		servers = cluster.Routers
	}

	session, err := mgo.DialWithInfo(dialInfo(servers, adminPassword))
	if err != nil {
		return serviceadapter.Binding{}, err
	}
	defer session.Close()

	// add user to admin database with admin privileges
	user := &mgo.User{
		Username: username,
		Password: password,
		Roles: []mgo.Role{
			mgo.RoleUserAdmin,
			mgo.RoleDBAdmin,
			mgo.RoleReadWrite,
		},
		OtherDBRoles: map[string][]mgo.Role{
			defaultDB: {
				mgo.RoleUserAdmin,
				mgo.RoleDBAdmin,
				mgo.RoleReadWrite,
			},
		},
	}

	if err = session.DB(adminDB).UpsertUser(user); err != nil {
		return serviceadapter.Binding{}, err
	}

	url := fmt.Sprintf("mongodb://%s:%s@%s/%s?authSource=admin",
		username,
		password,
		strings.Join(servers, ","),
		defaultDB,
	)

	b.logf("url: %s", url)
	b.logf("username: %s", username)
	b.logf("password: %s", password)

	return serviceadapter.Binding{
		Credentials: map[string]interface{}{
			"username": username,
			"password": password,
			"database": defaultDB,
			"servers":  servers,
			"uri":      url,
		},
	}, nil
}

func (Binder) DeleteBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) error {

	// create an admin level user
	username := mkUsername(bindingID)
	properties := manifest.Properties["mongo_ops"].(map[interface{}]interface{})
	adminPassword := properties["admin_password"].(string)

	servers := make([]string, len(deploymentTopology["mongod_node"]))
	for i, node := range deploymentTopology["mongod_node"] {
		servers[i] = fmt.Sprintf("%s:28000", node)
	}

	session, err := mgo.DialWithInfo(dialInfo(servers, adminPassword))
	if err != nil {
		return err
	}
	defer session.Close()

	return session.DB(adminDB).RemoveUser(username)
}

func dialInfo(addrs []string, adminPassword string) *mgo.DialInfo {
	return &mgo.DialInfo{
		Addrs:     addrs,
		Username:  "admin",
		Password:  adminPassword,
		Mechanism: "SCRAM-SHA-1",
		Database:  adminDB,
		FailFast:  true,
	}
}

func mkUsername(binddingID string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(binddingID))
	return fmt.Sprintf("pcf_%x", md5.Sum([]byte(b64)))
}
