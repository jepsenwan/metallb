// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"net"
	"reflect"
	"sort"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
)

type service interface {
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

type controller struct {
	myIP   net.IP
	myNode string
	client service

	config *config.Config
	peers  []*peer
	svcAds map[string][]*bgp.Advertisement
}

type peer struct {
	cfg *config.Peer
	bgp *bgp.Session
}

func (c *controller) SetBalancer(name string, svc *v1.Service, eps *v1.Endpoints) error {
	if svc == nil {
		return c.deleteBalancer(name, "service deleted")
	}

	glog.Infof("%s: start update", name)
	defer glog.Infof("%s: end update", name)

	if svc.Spec.Type != "LoadBalancer" {
		glog.Infof("%s: not a LoadBalancer", name)
		return nil
	}

	if c.config == nil {
		glog.Infof("%s: skipped, waiting for config", name)
		return nil
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		glog.Infof("%s: no IP allocated by controller", name)
		return c.deleteBalancer(name, "no IP allocated by controller")
	}

	// Should we advertise? Yes, if externalTrafficPolicy is Cluster,
	// or Local && there's a ready local endpoint.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !c.nodeHasHealthyEndpoint(eps) {
		glog.Infof("%s: externalTrafficPolicy is Local, and no healthy local endpoints", name)
		return c.deleteBalancer(name, "no healthy local endpoints")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP).To4()
	if lbIP == nil {
		glog.Errorf("%s: invalid LoadBalancer IP %q", name, svc.Status.LoadBalancer.Ingress[0].IP)
		return c.deleteBalancer(name, "invalid IP allocated by balancer")
	}

	// Find the advertisement configuration for the IP
	var ads []config.Advertisement
findAds:
	for _, pool := range c.config.Pools {
		for _, cidr := range pool.CIDR {
			if cidr.Contains(lbIP) {
				ads = pool.Advertisements
				break findAds
			}
		}
	}

	c.svcAds[name] = nil
	for _, adCfg := range ads {
		m := net.CIDRMask(adCfg.AggregationLength, 32)
		ad := &bgp.Advertisement{
			Prefix: &net.IPNet{
				IP:   lbIP.Mask(m),
				Mask: m,
			},
			NextHop:   c.myIP,
			LocalPref: adCfg.LocalPref,
		}
		for comm := range adCfg.Communities {
			ad.Communities = append(ad.Communities, comm)
		}
		sort.Slice(ad.Communities, func(i, j int) bool { return ad.Communities[i] < ad.Communities[j] })
		c.svcAds[name] = append(c.svcAds[name], ad)
	}

	glog.Infof("%s: announcable, making %d advertisements", name, len(c.svcAds[name]))

	if err := c.updateAds(); err != nil {
		return err
	}

	return nil
}

func (c *controller) nodeHasHealthyEndpoint(eps *v1.Endpoints) bool {
	ready := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil || *ep.NodeName != c.myNode {
				continue
			}
			if _, ok := ready[ep.IP]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep.IP] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep.IP] = false
		}
	}

	for _, r := range ready {
		if r {
			// At least one fully healthy endpoint on this machine.
			return true
		}
	}
	return false
}

func (c *controller) updateAds() error {
	var allAds []*bgp.Advertisement
	for _, ads := range c.svcAds {
		// This list might contain duplicates, but that's fine,
		// they'll get compacted by the session code when it's
		// calculating advertisements.
		//
		// TODO: be more intelligent about compacting advertisements
		// and detecting conflicting advertisements.
		allAds = append(allAds, ads...)
	}
	for _, peer := range c.peers {
		if err := peer.bgp.Set(allAds...); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) deleteBalancer(name, reason string) error {
	if _, ok := c.svcAds[name]; !ok {
		return nil
	}
	glog.Infof("%s: stopping announcements, %s", name, reason)
	delete(c.svcAds, name)
	return c.updateAds()
}

func (c *controller) SetConfig(cfg *config.Config) error {
	glog.Infof("Start config update")
	defer glog.Infof("End config update")

	newPeers := make([]*peer, 0, len(cfg.Peers))
newPeers:
	for _, p := range cfg.Peers {
		for i, ep := range c.peers {
			if ep == nil {
				continue
			}
			if reflect.DeepEqual(&p, ep.cfg) {
				newPeers = append(newPeers, ep)
				c.peers[i] = nil
				continue newPeers
			}
		}
		// No existing peers match, create a new one.
		newPeers = append(newPeers, &peer{
			cfg: &p,
		})
	}

	c.config = cfg
	oldPeers := c.peers
	c.peers = newPeers

	for _, p := range oldPeers {
		if p == nil {
			continue
		}
		glog.Infof("Peer %q deconfigured, closing BGP session", p.cfg.Addr)
		if err := p.bgp.Close(); err != nil {
			glog.Warningf("Shutting down BGP session to %q: %s", p.cfg.Addr, err)
		}
	}

	var errs []error
	for _, p := range c.peers {
		if p.bgp != nil {
			continue
		}

		glog.Infof("Peer %q configured, starting BGP session", p.cfg.Addr)
		s, err := bgp.New(fmt.Sprintf("%s:179", p.cfg.Addr), p.cfg.MyASN, net.ParseIP("192.168.18.65"), p.cfg.ASN, p.cfg.HoldTime)
		if err != nil {
			errs = append(errs, fmt.Errorf("Creating BGP session to %q: %s", p.cfg.Addr, err))
		} else {
			p.bgp = s
		}
	}
	if len(errs) != 0 {
		for _, err := range errs {
			glog.Error(err)
		}
		return fmt.Errorf("%d new BGP sessions failed to start", len(errs))
	}

	return nil
}

func (c *controller) MarkSynced() {}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	master := flag.String("master", "", "master url")
	myIPstr := flag.String("node-ip", "", "IP address of this Kubernetes node")
	myNode := flag.String("node-name", "", "Name of this Kubernetes node")
	flag.Parse()

	myIP := net.ParseIP(*myIPstr).To4()
	if myIP == nil {
		glog.Fatalf("Invalid --node-ip %q, must be an IPv4 address", *myIPstr)
	}

	if *myNode == "" {
		glog.Fatalf("Must specify --node-name")
	}

	c := &controller{
		myIP:   myIP,
		myNode: *myNode,
		svcAds: map[string][]*bgp.Advertisement{},
	}

	client, err := k8s.NewClient("metallb-bgp-speaker", *master, *kubeconfig, c, true)
	if err != nil {
		glog.Fatalf("Error getting k8s client: %s", err)
	}

	c.client = client

	glog.Fatal(client.Run())
}
