// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package nodem

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/clientv3"
	"github.com/goodrain/rainbond/cmd"
	"github.com/goodrain/rainbond/cmd/node/option"
	"github.com/goodrain/rainbond/node/api"
	"github.com/goodrain/rainbond/node/nodem/client"
	"github.com/goodrain/rainbond/node/nodem/controller"
	"github.com/goodrain/rainbond/node/nodem/healthy"
	"github.com/goodrain/rainbond/node/nodem/info"
	"github.com/goodrain/rainbond/node/nodem/monitor"
	"github.com/goodrain/rainbond/node/nodem/taskrun"
	"github.com/goodrain/rainbond/util"
)

//NodeManager node manager
type NodeManager struct {
	client.HostNode
	ctx        context.Context
	cancel     context.CancelFunc
	cluster    client.ClusterClient
	monitor    monitor.Manager
	healthy    healthy.Manager
	controller controller.Manager
	taskrun    taskrun.Manager
	cfg        *option.Conf
	apim       *api.Manager
}

//NewNodeManager new a node manager
func NewNodeManager(conf *option.Conf) (*NodeManager, error) {
	etcdcli, err := clientv3.New(conf.Etcd)
	if err != nil {
		return nil, err
	}
	controller := controller.NewLinuxManager(conf)
	taskrun, err := taskrun.Newmanager(conf, etcdcli)
	if err != nil {
		return nil, err
	}
	cluster := client.NewClusterClient(conf, etcdcli)
	monitor, err := monitor.CreateManager(conf)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	nodem := &NodeManager{
		cfg:        conf,
		ctx:        ctx,
		cancel:     cancel,
		controller: controller,
		taskrun:    taskrun,
		cluster:    cluster,
		monitor:    monitor,
	}
	return nodem, nil
}

//AddAPIManager AddApiManager
func (n *NodeManager) AddAPIManager(apim *api.Manager) error {
	n.apim = apim
	return n.monitor.SetAPIRoute(apim)
}

//Start start
func (n *NodeManager) Start(errchan chan error) error {
	if err := n.init(); err != nil {
		return err
	}
	if err := n.controller.Start(); err != nil {
		return fmt.Errorf("start node controller error,%s", err.Error())
	}
	// services, err := n.controller.GetAllService()
	// if err != nil {
	// 	return fmt.Errorf("get all services error,%s", err.Error())
	// }
	// if err := n.healthy.AddServices(services); err != nil {
	// 	return fmt.Errorf("get all services error,%s", err.Error())
	// }
	// if err := n.healthy.Start(); err != nil {
	// 	return fmt.Errorf("node healty start error,%s", err.Error())
	// }
	go n.monitor.Start(errchan)
	go n.taskrun.Start(errchan)
	go n.heartbeat()
	return nil
}

//Stop Stop
func (n *NodeManager) Stop() {
	n.cancel()
	n.cluster.DownNode(&n.HostNode)
	if n.taskrun != nil {
		n.taskrun.Stop()
	}
	if n.controller != nil {
		n.controller.Stop()
	}
	if n.monitor != nil {
		n.monitor.Stop()
	}
	if n.healthy != nil {
		n.healthy.Stop()
	}
}

//checkNodeHealthy check current node healthy.
//only healthy can controller other service start
func (n *NodeManager) checkNodeHealthy() error {
	return nil
}

func (n *NodeManager) heartbeat() {
	util.Exec(n.ctx, func() error {
		if err := n.cluster.UpdateStatus(&n.HostNode); err != nil {
			logrus.Errorf("update node status error %s", err.Error())
		}
		logrus.Info("update node status success")
		return nil
	}, time.Second*time.Duration(n.cfg.TTL))
}

//init node init
func (n *NodeManager) init() error {
	uid, err := util.ReadHostID(n.cfg.HostIDFile)
	if err != nil {
		return fmt.Errorf("Get host id error:%s", err.Error())
	}
	node, err := n.cluster.GetNode(uid)
	if err != nil {
		return err
	}
	if node == nil {
		node, err = n.getCurrentNode(uid)
		if err != nil {
			return err
		}
	}
	node.NodeStatus.NodeInfo = info.GetSystemInfo()
	node.Role = strings.Split(n.cfg.NodeRule, ",")
	if node.Labels == nil || len(node.Labels) < 1 {
		node.Labels = map[string]string{}
	}
	for _, rule := range node.Role {
		node.Labels["rainbond_node_rule_"+rule] = "true"
	}
	if node.HostName == "" {
		hostname, _ := os.Hostname()
		node.HostName = hostname
	}
	if node.ClusterNode.PID == "" {
		node.ClusterNode.PID = strconv.Itoa(os.Getpid())
	}
	node.Labels["rainbond_node_hostname"] = node.HostName
	node.Labels["rainbond_node_ip"] = node.InternalIP
	node.UpdataCondition(client.NodeCondition{
		Type:               client.NodeInit,
		Status:             client.ConditionTrue,
		LastHeartbeatTime:  time.Now(),
		LastTransitionTime: time.Now(),
	})
	node.Mode = n.cfg.RunMode
	n.HostNode = *node
	if node.AvailableMemory == 0 {
		node.AvailableMemory = int64(node.NodeStatus.NodeInfo.MemorySize)
	}
	if node.AvailableCPU == 0 {
		node.AvailableCPU = int64(runtime.NumCPU())
	}
	node.Version = cmd.GetVersion()
	return nil
}

//UpdateNodeStatus UpdateNodeStatus
func (n *NodeManager) UpdateNodeStatus() error {
	return n.cluster.UpdateStatus(&n.HostNode)
}

//getCurrentNode get current node info
func (n *NodeManager) getCurrentNode(uid string) (*client.HostNode, error) {
	if n.cfg.HostIP == "" {
		ip, err := util.LocalIP()
		if err != nil {
			return nil, err
		}
		n.cfg.HostIP = ip.String()
	}
	node := CreateNode(uid, n.cfg.HostIP)
	return &node, nil
}

//GetCurrentNode get current node
func (n *NodeManager) GetCurrentNode() *client.HostNode {
	return &n.HostNode
}

//CreateNode new node
func CreateNode(nodeID, ip string) client.HostNode {
	HostNode := client.HostNode{
		ID: nodeID,
		ClusterNode: client.ClusterNode{
			PID: strconv.Itoa(os.Getpid()),
		},
		InternalIP: ip,
		ExternalIP: ip,
		CreateTime: time.Now(),
		NodeStatus: &client.NodeStatus{},
	}
	return HostNode
}
