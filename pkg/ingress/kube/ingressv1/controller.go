// Copyright (c) 2022 Alibaba Group Holding Ltd.
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

package ingressv1

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/credentials"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"
	kubeclient "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	ingress "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	networkingv1 "k8s.io/client-go/informers/networking/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	networkinglister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/alibaba/higress/pkg/ingress/kube/annotations"
	"github.com/alibaba/higress/pkg/ingress/kube/common"
	"github.com/alibaba/higress/pkg/ingress/kube/secret"
	"github.com/alibaba/higress/pkg/ingress/kube/util"
	. "github.com/alibaba/higress/pkg/ingress/log"
)

var (
	_ common.IngressController = &controller{}

	// follow specification of ingress-nginx
	defaultPathType = ingress.PathTypePrefix
)

type controller struct {
	queue                   workqueue.RateLimitingInterface
	virtualServiceHandlers  []model.EventHandler
	gatewayHandlers         []model.EventHandler
	destinationRuleHandlers []model.EventHandler
	envoyFilterHandlers     []model.EventHandler

	options common.Options

	mutex sync.RWMutex
	// key: namespace/name
	ingresses map[string]*ingress.Ingress

	ingressInformer cache.SharedInformer
	ingressLister   networkinglister.IngressLister
	serviceInformer cache.SharedInformer
	serviceLister   listerv1.ServiceLister
	classes         networkingv1.IngressClassInformer

	secretController secret.Controller

	statusSyncer *statusSyncer
}

// NewController creates a new Kubernetes controller
func NewController(localKubeClient, client kubeclient.Client, options common.Options, secretController secret.Controller) common.IngressController {
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter())

	ingressInformer := client.KubeInformer().Networking().V1().Ingresses()
	serviceInformer := client.KubeInformer().Core().V1().Services()

	classes := client.KubeInformer().Networking().V1().IngressClasses()
	classes.Informer()

	c := &controller{
		options:          options,
		queue:            q,
		ingresses:        make(map[string]*ingress.Ingress),
		ingressInformer:  ingressInformer.Informer(),
		ingressLister:    ingressInformer.Lister(),
		classes:          classes,
		serviceInformer:  serviceInformer.Informer(),
		serviceLister:    serviceInformer.Lister(),
		secretController: secretController,
	}

	handler := controllers.LatestVersionHandlerFuncs(controllers.EnqueueForSelf(q))
	c.ingressInformer.AddEventHandler(handler)

	if options.EnableStatus {
		c.statusSyncer = newStatusSyncer(localKubeClient, client, c, options.SystemNamespace)
	} else {
		IngressLog.Infof("Disable status update for cluster %s", options.ClusterId)
	}

	return c
}

func (c *controller) ServiceLister() listerv1.ServiceLister {
	return c.serviceLister
}

func (c *controller) SecretLister() listerv1.SecretLister {
	return c.secretController.Lister()
}

func (c *controller) Run(stop <-chan struct{}) {
	if c.statusSyncer != nil {
		go c.statusSyncer.run(stop)
	}
	go c.secretController.Run(stop)

	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(stop, c.HasSynced) {
		IngressLog.Errorf("Failed to sync ingress controller cache for cluster %s", c.options.ClusterId)
		return
	}
	go wait.Until(c.worker, time.Second, stop)
	<-stop
}

func (c *controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)
	ingressNamespacedName := key.(types.NamespacedName)
	IngressLog.Debugf("ingress %s push to queue", ingressNamespacedName)
	if err := c.onEvent(ingressNamespacedName); err != nil {
		IngressLog.Errorf("error processing ingress item (%v) (retrying): %v, cluster: %s", key, err, c.options.ClusterId)
		c.queue.AddRateLimited(key)
	} else {
		c.queue.Forget(key)
	}
	return true
}

func (c *controller) onEvent(namespacedName types.NamespacedName) error {
	event := model.EventUpdate
	ing, err := c.ingressLister.Ingresses(namespacedName.Namespace).Get(namespacedName.Name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			event = model.EventDelete
			c.mutex.Lock()
			ing = c.ingresses[namespacedName.String()]
			delete(c.ingresses, namespacedName.String())
			c.mutex.Unlock()
		} else {
			return err
		}
	}

	// ingress deleted, and it is not processed before
	if ing == nil {
		return nil
	}

	IngressLog.Debugf("ingress: %s, event: %s", namespacedName, event)

	// we should check need process only when event is not delete,
	// if it is delete event, and previously processed, we need to process too.
	if event != model.EventDelete {
		shouldProcess, err := c.shouldProcessIngressUpdate(ing)
		if err != nil {
			return err
		}
		if !shouldProcess {
			IngressLog.Infof("no need process, ingress %s", namespacedName)
			return nil
		}
	}

	drmetadata := config.Meta{
		Name:             ing.Name + "-" + "destinationrule",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.DestinationRule,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	vsmetadata := config.Meta{
		Name:             ing.Name + "-" + "virtualservice",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.VirtualService,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	efmetadata := config.Meta{
		Name:             ing.Name + "-" + "envoyfilter",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.EnvoyFilter,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	gatewaymetadata := config.Meta{
		Name:             ing.Name + "-" + "gateway",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.Gateway,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}

	for _, f := range c.destinationRuleHandlers {
		f(config.Config{Meta: drmetadata}, config.Config{Meta: drmetadata}, event)
	}

	for _, f := range c.virtualServiceHandlers {
		f(config.Config{Meta: vsmetadata}, config.Config{Meta: vsmetadata}, event)
	}

	for _, f := range c.envoyFilterHandlers {
		f(config.Config{Meta: efmetadata}, config.Config{Meta: efmetadata}, event)
	}

	for _, f := range c.gatewayHandlers {
		f(config.Config{Meta: gatewaymetadata}, config.Config{Meta: gatewaymetadata}, event)
	}

	return nil
}

func (c *controller) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	switch kind {
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	case gvk.DestinationRule:
		c.destinationRuleHandlers = append(c.destinationRuleHandlers, f)
	case gvk.EnvoyFilter:
		c.envoyFilterHandlers = append(c.envoyFilterHandlers, f)
	}
}

func (c *controller) SetWatchErrorHandler(handler func(r *cache.Reflector, err error)) error {
	var errs error
	if err := c.serviceInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := c.ingressInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := c.secretController.Informer().SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := c.classes.Informer().SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	return errs
}

func (c *controller) HasSynced() bool {
	return c.ingressInformer.HasSynced() && c.serviceInformer.HasSynced() &&
		c.classes.Informer().HasSynced() &&
		c.secretController.HasSynced()
}

func (c *controller) List() []config.Config {
	out := make([]config.Config, 0, len(c.ingresses))

	for _, raw := range c.ingressInformer.GetStore().List() {
		ing, ok := raw.(*ingress.Ingress)
		if !ok {
			continue
		}

		if should, err := c.shouldProcessIngress(ing); !should || err != nil {
			continue
		}

		copiedConfig := ing.DeepCopy()
		setDefaultMSEIngressOptionalField(copiedConfig)

		outConfig := config.Config{
			Meta: config.Meta{
				Name:              copiedConfig.Name,
				Namespace:         copiedConfig.Namespace,
				Annotations:       common.CreateOrUpdateAnnotations(copiedConfig.Annotations, c.options),
				Labels:            copiedConfig.Labels,
				CreationTimestamp: copiedConfig.CreationTimestamp.Time,
			},
			Spec: copiedConfig.Spec,
		}

		out = append(out, outConfig)
	}

	common.RecordIngressNumber(c.options.ClusterId, len(out))
	return out
}

func extractTLSSecretName(host string, tls []ingress.IngressTLS) string {
	if len(tls) == 0 {
		return ""
	}

	for _, t := range tls {
		match := false
		for _, h := range t.Hosts {
			if h == host {
				match = true
			}
		}

		if match {
			return t.SecretName
		}
	}

	return ""
}

func (c *controller) ConvertGateway(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	// Ignore canary config.
	if wrapper.AnnotationsConfig.IsCanary() {
		return nil
	}

	cfg := wrapper.Config
	ingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(ingressV1.Rules) == 0 && ingressV1.DefaultBackend == nil {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, either `defaultBackend` or `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}

	for _, rule := range ingressV1.Rules {
		cleanHost := common.CleanHost(rule.Host)
		// Need create builder for every rule.
		domainBuilder := &common.IngressDomainBuilder{
			ClusterId: c.options.ClusterId,
			Protocol:  common.HTTP,
			Host:      rule.Host,
			Ingress:   cfg,
			Event:     common.Normal,
		}

		// Extract the previous gateway and builder
		wrapperGateway, exist := convertOptions.Gateways[rule.Host]
		preDomainBuilder, _ := convertOptions.IngressDomainCache.Valid[rule.Host]
		if !exist {
			wrapperGateway = &common.WrapperGateway{
				Gateway:       &networking.Gateway{},
				WrapperConfig: wrapper,
				ClusterId:     c.options.ClusterId,
				Host:          rule.Host,
			}
			if c.options.GatewaySelectorKey != "" {
				wrapperGateway.Gateway.Selector = map[string]string{c.options.GatewaySelectorKey: c.options.GatewaySelectorValue}

			}
			wrapperGateway.Gateway.Servers = append(wrapperGateway.Gateway.Servers, &networking.Server{
				Port: &networking.Port{
					Number:   80,
					Protocol: string(protocol.HTTP),
					Name:     common.CreateConvertedName("http-80-ingress", c.options.ClusterId, cfg.Namespace, cfg.Name, cleanHost),
				},
				Hosts: []string{rule.Host},
			})

			// Add new gateway, builder
			convertOptions.Gateways[rule.Host] = wrapperGateway
			convertOptions.IngressDomainCache.Valid[rule.Host] = domainBuilder
		} else {
			// Fallback to get downstream tls from current ingress.
			if wrapperGateway.WrapperConfig.AnnotationsConfig.DownstreamTLS == nil {
				wrapperGateway.WrapperConfig.AnnotationsConfig.DownstreamTLS = wrapper.AnnotationsConfig.DownstreamTLS
			}
		}

		// There are no tls settings, so just skip.
		if len(ingressV1.TLS) == 0 {
			continue
		}

		// Get tls secret matching the rule host
		secretName := extractTLSSecretName(rule.Host, ingressV1.TLS)
		if secretName == "" {
			// There no matching secret, so just skip.
			continue
		}

		domainBuilder.Protocol = common.HTTPS
		domainBuilder.SecretName = path.Join(c.options.ClusterId, cfg.Namespace, secretName)

		// There is a matching secret and the gateway has already a tls secret.
		// We should report the duplicated tls secret event.
		if wrapperGateway.IsHTTPS() {
			domainBuilder.Event = common.DuplicatedTls
			domainBuilder.PreIngress = preDomainBuilder.Ingress
			convertOptions.IngressDomainCache.Invalid = append(convertOptions.IngressDomainCache.Invalid,
				domainBuilder.Build())
			continue
		}

		// Append https server
		wrapperGateway.Gateway.Servers = append(wrapperGateway.Gateway.Servers, &networking.Server{
			Port: &networking.Port{
				Number:   443,
				Protocol: string(protocol.HTTPS),
				Name:     common.CreateConvertedName("https-443-ingress", c.options.ClusterId, cfg.Namespace, cfg.Name, cleanHost),
			},
			Hosts: []string{rule.Host},
			Tls: &networking.ServerTLSSettings{
				Mode:           networking.ServerTLSSettings_SIMPLE,
				CredentialName: credentials.ToKubernetesIngressResource(c.options.RawClusterId, cfg.Namespace, secretName),
			},
		})

		// Update domain builder
		convertOptions.IngressDomainCache.Valid[rule.Host] = domainBuilder
	}

	return nil
}

func (c *controller) ConvertHTTPRoute(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	// Canary ingress will be processed in the end.
	if wrapper.AnnotationsConfig.IsCanary() {
		convertOptions.CanaryIngresses = append(convertOptions.CanaryIngresses, wrapper)
		return nil
	}

	cfg := wrapper.Config
	ingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(ingressV1.Rules) == 0 && ingressV1.DefaultBackend == nil {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, either `defaultBackend` or `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}

	if ingressV1.DefaultBackend != nil && ingressV1.DefaultBackend.Service != nil &&
		ingressV1.DefaultBackend.Service.Name != "" {
		convertOptions.HasDefaultBackend = true
	}

	// In one ingress, we will limit the rule conflict.
	// When the host, pathType, path of two rule are same, we think there is a conflict event.
	definedRules := sets.NewSet()

	// But in across ingresses case, we will restrict this limit.
	// When the host, path of two rule in different ingress are same, we think there is a conflict event.
	var tempHostAndPath []string
	for _, rule := range ingressV1.Rules {
		if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			IngressLog.Warnf("invalid ingress rule %s:%s for host %q in cluster %s, no paths defined", cfg.Namespace, cfg.Name, rule.Host, c.options.ClusterId)
			continue
		}

		wrapperVS, exist := convertOptions.VirtualServices[rule.Host]
		if !exist {
			wrapperVS = &common.WrapperVirtualService{
				VirtualService: &networking.VirtualService{
					Hosts: []string{rule.Host},
				},
				WrapperConfig: wrapper,
			}
			convertOptions.VirtualServices[rule.Host] = wrapperVS
		}

		// Record the latest app root for per host.
		redirect := wrapper.AnnotationsConfig.Redirect
		if redirect != nil && redirect.AppRoot != "" {
			wrapperVS.AppRoot = redirect.AppRoot
		}

		wrapperHttpRoutes := make([]*common.WrapperHTTPRoute, 0, len(rule.HTTP.Paths))
		for _, httpPath := range rule.HTTP.Paths {
			wrapperHttpRoute := &common.WrapperHTTPRoute{
				HTTPRoute:     &networking.HTTPRoute{},
				WrapperConfig: wrapper,
				Host:          rule.Host,
				ClusterId:     c.options.ClusterId,
			}
			httpMatch := &networking.HTTPMatchRequest{}

			path := httpPath.Path
			if wrapper.AnnotationsConfig.NeedRegexMatch() {
				wrapperHttpRoute.OriginPathType = common.Regex
				httpMatch.Uri = &networking.StringMatch{
					MatchType: &networking.StringMatch_Regex{Regex: httpPath.Path + ".*"},
				}
			} else {
				switch *httpPath.PathType {
				case ingress.PathTypeExact:
					wrapperHttpRoute.OriginPathType = common.Exact
					httpMatch.Uri = &networking.StringMatch{
						MatchType: &networking.StringMatch_Exact{Exact: httpPath.Path},
					}
				case ingress.PathTypePrefix:
					wrapperHttpRoute.OriginPathType = common.Prefix
					// borrow from implement of official istio code.
					if path == "/" {
						wrapperVS.ConfiguredDefaultBackend = true
						// Optimize common case of / to not needed regex
						httpMatch.Uri = &networking.StringMatch{
							MatchType: &networking.StringMatch_Prefix{Prefix: path},
						}
					} else {
						path = strings.TrimSuffix(path, "/")
						httpMatch.Uri = &networking.StringMatch{
							MatchType: &networking.StringMatch_Regex{Regex: regexp.QuoteMeta(path) + common.PrefixMatchRegex},
						}
					}
				}
			}
			wrapperHttpRoute.OriginPath = path
			wrapperHttpRoute.HTTPRoute.Match = []*networking.HTTPMatchRequest{httpMatch}
			wrapperHttpRoute.HTTPRoute.Name = common.GenerateUniqueRouteName(wrapperHttpRoute)

			ingressRouteBuilder := convertOptions.IngressRouteCache.New(wrapperHttpRoute)

			// host and path overlay check across different ingresses.
			hostAndPath := wrapperHttpRoute.BasePathFormat()
			if preIngress, exist := convertOptions.HostAndPath2Ingress[hostAndPath]; exist {
				ingressRouteBuilder.PreIngress = preIngress
				ingressRouteBuilder.Event = common.DuplicatedRoute
			}
			tempHostAndPath = append(tempHostAndPath, hostAndPath)

			// Two duplicated rules in the same ingress.
			if ingressRouteBuilder.Event == common.Normal {
				pathFormat := wrapperHttpRoute.PathFormat()
				if definedRules.Contains(pathFormat) {
					ingressRouteBuilder.PreIngress = cfg
					ingressRouteBuilder.Event = common.DuplicatedRoute
				}
				definedRules.Insert(pathFormat)
			}

			// backend service check
			var event common.Event
			wrapperHttpRoute.HTTPRoute.Route, event = c.backendToRouteDestination(&httpPath.Backend, cfg.Namespace, ingressRouteBuilder)

			if ingressRouteBuilder.Event != common.Normal {
				event = ingressRouteBuilder.Event
			}

			if event != common.Normal {
				common.IncrementInvalidIngress(c.options.ClusterId, event)
				ingressRouteBuilder.Event = event
			} else {
				wrapperHttpRoutes = append(wrapperHttpRoutes, wrapperHttpRoute)
			}

			convertOptions.IngressRouteCache.Add(ingressRouteBuilder)
		}

		for _, item := range tempHostAndPath {
			// We only record the first
			if _, exist := convertOptions.HostAndPath2Ingress[item]; !exist {
				convertOptions.HostAndPath2Ingress[item] = cfg
			}
		}

		old, f := convertOptions.HTTPRoutes[rule.Host]
		if f {
			old = append(old, wrapperHttpRoutes...)
			convertOptions.HTTPRoutes[rule.Host] = old
		} else {
			convertOptions.HTTPRoutes[rule.Host] = wrapperHttpRoutes
		}

		// Sort, exact -> prefix -> regex
		routes := convertOptions.HTTPRoutes[rule.Host]
		IngressLog.Debugf("routes of host %s is %v", rule.Host, routes)
		common.SortHTTPRoutes(routes)
	}

	return nil
}

func (c *controller) ApplyDefaultBackend(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	if wrapper.AnnotationsConfig.IsCanary() {
		return nil
	}

	cfg := wrapper.Config
	ingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}

	if ingressV1.DefaultBackend == nil {
		return nil
	}

	apply := func(host string, op func(vs *common.WrapperVirtualService, defaultRoute *common.WrapperHTTPRoute)) {
		wirecardVS, exist := convertOptions.VirtualServices[host]
		if !exist || !wirecardVS.ConfiguredDefaultBackend {
			if !exist {
				wirecardVS = &common.WrapperVirtualService{
					VirtualService: &networking.VirtualService{
						Hosts: []string{host},
					},
					WrapperConfig: wrapper,
				}
				convertOptions.VirtualServices[host] = wirecardVS
			}

			specDefaultBackend := c.createDefaultRoute(wrapper, ingressV1.DefaultBackend, host)
			if specDefaultBackend != nil {
				convertOptions.VirtualServices[host] = wirecardVS
				op(wirecardVS, specDefaultBackend)
			}
		}
	}

	// First process *
	apply("*", func(_ *common.WrapperVirtualService, defaultRoute *common.WrapperHTTPRoute) {
		var hasFound bool
		for _, httpRoute := range convertOptions.HTTPRoutes["*"] {
			if httpRoute.OriginPathType == common.Prefix && httpRoute.OriginPath == "/" {
				hasFound = true
				convertOptions.IngressRouteCache.Delete(httpRoute)

				httpRoute.HTTPRoute = defaultRoute.HTTPRoute
				httpRoute.WrapperConfig = defaultRoute.WrapperConfig
				convertOptions.IngressRouteCache.NewAndAdd(httpRoute)
			}
		}
		if !hasFound {
			convertOptions.HTTPRoutes["*"] = append(convertOptions.HTTPRoutes["*"], defaultRoute)
		}
	})

	for _, rule := range ingressV1.Rules {
		if rule.Host == "*" {
			continue
		}

		apply(rule.Host, func(vs *common.WrapperVirtualService, defaultRoute *common.WrapperHTTPRoute) {
			convertOptions.HTTPRoutes[rule.Host] = append(convertOptions.HTTPRoutes[rule.Host], defaultRoute)
			vs.ConfiguredDefaultBackend = true

			convertOptions.IngressRouteCache.NewAndAdd(defaultRoute)
		})
	}

	return nil
}

func (c *controller) ApplyCanaryIngress(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	byHeader, byWeight := wrapper.AnnotationsConfig.CanaryKind()

	cfg := wrapper.Config
	ingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(ingressV1.Rules) == 0 && ingressV1.DefaultBackend == nil {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, either `defaultBackend` or `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}

	for _, rule := range ingressV1.Rules {
		if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			IngressLog.Warnf("invalid ingress rule %s:%s for host %q in cluster %s, no paths defined", cfg.Namespace, cfg.Name, rule.Host, c.options.ClusterId)
			continue
		}

		routes, exist := convertOptions.HTTPRoutes[rule.Host]
		if !exist {
			continue
		}

		for _, httpPath := range rule.HTTP.Paths {
			path := httpPath.Path

			canary := &common.WrapperHTTPRoute{
				HTTPRoute:     &networking.HTTPRoute{},
				WrapperConfig: wrapper,
				Host:          rule.Host,
				ClusterId:     c.options.ClusterId,
			}
			httpMatch := &networking.HTTPMatchRequest{}

			if wrapper.AnnotationsConfig.NeedRegexMatch() {
				canary.OriginPathType = common.Regex
				httpMatch.Uri = &networking.StringMatch{
					MatchType: &networking.StringMatch_Regex{Regex: httpPath.Path + ".*"},
				}
			} else {
				switch *httpPath.PathType {
				case ingress.PathTypeExact:
					canary.OriginPathType = common.Exact
					httpMatch.Uri = &networking.StringMatch{
						MatchType: &networking.StringMatch_Exact{Exact: httpPath.Path},
					}
				case ingress.PathTypePrefix:
					canary.OriginPathType = common.Prefix
					// borrow from implement of official istio code.
					if path == "/" {
						// Optimize common case of / to not needed regex
						httpMatch.Uri = &networking.StringMatch{
							MatchType: &networking.StringMatch_Prefix{Prefix: path},
						}
					} else {
						path = strings.TrimSuffix(path, "/")
						httpMatch.Uri = &networking.StringMatch{
							MatchType: &networking.StringMatch_Regex{Regex: regexp.QuoteMeta(path) + common.PrefixMatchRegex},
						}
					}
				}
			}
			canary.OriginPath = path
			canary.HTTPRoute.Match = []*networking.HTTPMatchRequest{httpMatch}
			canary.HTTPRoute.Name = common.GenerateUniqueRouteName(canary)

			ingressRouteBuilder := convertOptions.IngressRouteCache.New(canary)
			// backend service check
			var event common.Event
			canary.HTTPRoute.Route, event = c.backendToRouteDestination(&httpPath.Backend, cfg.Namespace, ingressRouteBuilder)
			if event != common.Normal {
				common.IncrementInvalidIngress(c.options.ClusterId, event)
				ingressRouteBuilder.Event = event
				convertOptions.IngressRouteCache.Add(ingressRouteBuilder)
				continue
			}

			canaryConfig := wrapper.AnnotationsConfig.Canary
			if byWeight {
				canary.HTTPRoute.Route[0].Weight = int32(canaryConfig.Weight)
			}

			pos := 0
			var targetRoute *common.WrapperHTTPRoute
			for _, route := range routes {
				if isCanaryRoute(canary, route) {
					targetRoute = route
					// Header, Cookie
					if byHeader {
						IngressLog.Debug("Insert canary route by header")
						annotations.ApplyByHeader(canary.HTTPRoute, route.HTTPRoute, canary.WrapperConfig.AnnotationsConfig)
						canary.HTTPRoute.Name = common.GenerateUniqueRouteName(canary)
					} else {
						IngressLog.Debug("Merge canary route by weight")
						if route.WeightTotal == 0 {
							route.WeightTotal = int32(canaryConfig.WeightTotal)
						}
						annotations.ApplyByWeight(canary.HTTPRoute, route.HTTPRoute, canary.WrapperConfig.AnnotationsConfig)
					}

					break
				}
				pos += 1
			}

			IngressLog.Debugf("Canary route is %v", canary)
			if targetRoute == nil {
				continue
			}

			if byHeader {
				// Inherit policy from normal route
				canary.WrapperConfig.AnnotationsConfig.Auth = targetRoute.WrapperConfig.AnnotationsConfig.Auth

				routes = append(routes[:pos+1], routes[pos:]...)
				routes[pos] = canary
				convertOptions.HTTPRoutes[rule.Host] = routes

				// Recreate route name.
				ingressRouteBuilder.RouteName = common.GenerateUniqueRouteName(canary)
				convertOptions.IngressRouteCache.Add(ingressRouteBuilder)
			} else {
				convertOptions.IngressRouteCache.Update(targetRoute)
			}
		}
	}
	return nil
}

func (c *controller) ConvertTrafficPolicy(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	if !wrapper.AnnotationsConfig.NeedTrafficPolicy() {
		return nil
	}

	cfg := wrapper.Config
	ingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(ingressV1.Rules) == 0 && ingressV1.DefaultBackend == nil {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, either `defaultBackend` or `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}

	if ingressV1.DefaultBackend != nil {
		serviceKey, err := c.createServiceKey(ingressV1.DefaultBackend.Service, cfg.Namespace)
		if err != nil {
			IngressLog.Errorf("ignore default service %s within ingress %s/%s", serviceKey.Name, cfg.Namespace, cfg.Name)
		} else {
			if _, exist := convertOptions.Service2TrafficPolicy[serviceKey]; !exist {
				convertOptions.Service2TrafficPolicy[serviceKey] = &common.WrapperTrafficPolicy{
					TrafficPolicy: &networking.TrafficPolicy_PortTrafficPolicy{
						Port: &networking.PortSelector{
							Number: uint32(serviceKey.Port),
						},
					},
					WrapperConfig: wrapper,
				}
			}
		}
	}

	for _, rule := range ingressV1.Rules {
		if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			continue
		}

		for _, httpPath := range rule.HTTP.Paths {
			if httpPath.Backend.Service == nil {
				continue
			}

			serviceKey, err := c.createServiceKey(httpPath.Backend.Service, cfg.Namespace)
			if err != nil {
				IngressLog.Errorf("ignore service %s within ingress %s/%s", serviceKey.Name, cfg.Namespace, cfg.Name)
				continue
			}

			if _, exist := convertOptions.Service2TrafficPolicy[serviceKey]; exist {
				continue
			}

			convertOptions.Service2TrafficPolicy[serviceKey] = &common.WrapperTrafficPolicy{
				TrafficPolicy: &networking.TrafficPolicy_PortTrafficPolicy{
					Port: &networking.PortSelector{
						Number: uint32(serviceKey.Port),
					},
				},
				WrapperConfig: wrapper,
			}
		}
	}

	return nil
}

func (c *controller) createDefaultRoute(wrapper *common.WrapperConfig, backend *ingress.IngressBackend, host string) *common.WrapperHTTPRoute {
	if backend == nil || backend.Service == nil || backend.Service.Name == "" {
		return nil
	}

	service := backend.Service
	namespace := wrapper.Config.Namespace

	port := &networking.PortSelector{}
	if service.Port.Number > 0 {
		port.Number = uint32(service.Port.Number)
	} else {
		resolvedPort, err := resolveNamedPort(service, namespace, c.serviceLister)
		if err != nil {
			return nil
		}
		port.Number = uint32(resolvedPort)
	}

	routeDestination := []*networking.HTTPRouteDestination{
		{
			Destination: &networking.Destination{
				Host: util.CreateServiceFQDN(namespace, service.Name),
				Port: port,
			},
			Weight: 100,
		},
	}

	route := &common.WrapperHTTPRoute{
		HTTPRoute: &networking.HTTPRoute{
			Route: routeDestination,
		},
		WrapperConfig:    wrapper,
		ClusterId:        c.options.ClusterId,
		Host:             host,
		IsDefaultBackend: true,
		OriginPathType:   common.Prefix,
		OriginPath:       "/",
	}
	route.HTTPRoute.Name = common.GenerateUniqueRouteNameWithSuffix(route, "default")

	return route
}

func (c *controller) createServiceKey(service *ingress.IngressServiceBackend, namespace string) (common.ServiceKey, error) {
	serviceKey := common.ServiceKey{}
	if service == nil || service.Name == "" {
		return serviceKey, errors.New("service name is empty")
	}

	var port int32
	var err error
	if service.Port.Number > 0 {
		port = service.Port.Number
	} else {
		port, err = resolveNamedPort(service, namespace, c.serviceLister)
		if err != nil {
			return serviceKey, err
		}
	}

	return common.ServiceKey{
		Namespace: namespace,
		Name:      service.Name,
		Port:      port,
	}, nil
}

func isCanaryRoute(canary, route *common.WrapperHTTPRoute) bool {
	return !strings.HasSuffix(route.HTTPRoute.Name, "-canary") && canary.OriginPath == route.OriginPath &&
		canary.OriginPathType == route.OriginPathType
}

func (c *controller) backendToRouteDestination(backend *ingress.IngressBackend, namespace string,
	builder *common.IngressRouteBuilder) ([]*networking.HTTPRouteDestination, common.Event) {
	if backend == nil || backend.Service == nil {
		return nil, common.InvalidBackendService
	}

	service := backend.Service
	if service.Name == "" {
		return nil, common.InvalidBackendService
	}

	builder.PortName = service.Port.Name

	port := &networking.PortSelector{}
	if service.Port.Number > 0 {
		port.Number = uint32(service.Port.Number)
	} else {
		resolvedPort, err := resolveNamedPort(service, namespace, c.serviceLister)
		if err != nil {
			return nil, common.PortNameResolveError
		}
		port.Number = uint32(resolvedPort)
	}

	builder.ServiceList = []model.BackendService{
		{
			Namespace: namespace,
			Name:      service.Name,
			Port:      port.Number,
			Weight:    100,
		},
	}

	return []*networking.HTTPRouteDestination{
		{
			Destination: &networking.Destination{
				Host: util.CreateServiceFQDN(namespace, service.Name),
				Port: port,
			},
			Weight: 100,
		},
	}, common.Normal
}

func resolveNamedPort(service *ingress.IngressServiceBackend, namespace string, serviceLister listerv1.ServiceLister) (int32, error) {
	svc, err := serviceLister.Services(namespace).Get(service.Name)
	if err != nil {
		return 0, err
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == service.Port.Name {
			return port.Port, nil
		}
	}
	return 0, common.ErrNotFound
}

func (c *controller) shouldProcessIngressWithClass(ingress *ingress.Ingress, ingressClass *ingress.IngressClass) bool {
	if class, exists := ingress.Annotations[kube.IngressClassAnnotation]; exists {
		switch c.options.IngressClass {
		case "":
			return true
		case common.DefaultIngressClass:
			return class == "" || class == common.DefaultIngressClass
		default:
			return c.options.IngressClass == class
		}
	} else if ingressClass != nil {
		switch c.options.IngressClass {
		case "":
			return true
		default:
			return c.options.IngressClass == ingressClass.Name
		}
	} else {
		ingressClassName := ingress.Spec.IngressClassName
		switch c.options.IngressClass {
		case "":
			return true
		case common.DefaultIngressClass:
			return ingressClassName == nil || *ingressClassName == "" ||
				*ingressClassName == common.DefaultIngressClass
		default:
			return ingressClassName != nil && *ingressClassName == c.options.IngressClass
		}
	}
}

func (c *controller) shouldProcessIngress(i *ingress.Ingress) (bool, error) {
	var class *ingress.IngressClass
	if c.classes != nil && i.Spec.IngressClassName != nil {
		classCache, err := c.classes.Lister().Get(*i.Spec.IngressClassName)
		if err != nil && !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get ingress class %v from cluster %s: %v", i.Spec.IngressClassName, c.options.ClusterId, err)
		}
		class = classCache
	}

	// first check ingress class
	if c.shouldProcessIngressWithClass(i, class) {
		// then check namespace
		switch c.options.WatchNamespace {
		case "":
			return true, nil
		default:
			return c.options.WatchNamespace == i.Namespace, nil
		}
	}

	return false, nil
}

// shouldProcessIngressUpdate checks whether we should renotify registered handlers about an update event
func (c *controller) shouldProcessIngressUpdate(ing *ingress.Ingress) (bool, error) {
	shouldProcess, err := c.shouldProcessIngress(ing)
	if err != nil {
		return false, err
	}

	namespacedName := ing.Namespace + "/" + ing.Name
	if shouldProcess {
		// record processed ingress
		c.mutex.Lock()
		preConfig, exist := c.ingresses[namespacedName]
		c.ingresses[namespacedName] = ing
		c.mutex.Unlock()

		// We only care about annotations, labels and spec.
		if exist {
			if !reflect.DeepEqual(preConfig.Annotations, ing.Annotations) {
				IngressLog.Debugf("Annotations of ingress %s changed, should process.", namespacedName)
				return true, nil
			}
			if !reflect.DeepEqual(preConfig.Labels, ing.Labels) {
				IngressLog.Debugf("Labels of ingress %s changed, should process.", namespacedName)
				return true, nil
			}
			if !reflect.DeepEqual(preConfig.Spec, ing.Spec) {
				IngressLog.Debugf("Spec of ingress %s changed, should process.", namespacedName)
				return true, nil
			}

			return false, nil
		}

		IngressLog.Debugf("First receive relative ingress %s, should process.", namespacedName)
		return true, nil
	}

	c.mutex.Lock()
	_, preProcessed := c.ingresses[namespacedName]
	// previous processed but should not currently, delete it
	if preProcessed && !shouldProcess {
		delete(c.ingresses, namespacedName)
	}
	c.mutex.Unlock()

	return preProcessed, nil
}

// setDefaultMSEIngressOptionalField sets a default value for optional fields when is not defined.
func setDefaultMSEIngressOptionalField(ing *ingress.Ingress) {
	for idx, tls := range ing.Spec.TLS {
		if len(tls.Hosts) == 0 {
			ing.Spec.TLS[idx].Hosts = []string{common.DefaultHost}
		}
	}

	for idx, rule := range ing.Spec.Rules {
		if rule.IngressRuleValue.HTTP == nil {
			continue
		}

		if rule.Host == "" {
			ing.Spec.Rules[idx].Host = common.DefaultHost
		}

		for innerIdx := range rule.IngressRuleValue.HTTP.Paths {
			p := &rule.IngressRuleValue.HTTP.Paths[innerIdx]

			if p.Path == "" {
				p.Path = common.DefaultPath
			}

			if p.PathType == nil {
				p.PathType = &defaultPathType
				// for old k8s version
				if !annotations.NeedRegexMatch(ing.Annotations) {
					if strings.HasSuffix(p.Path, ".*") {
						p.Path = strings.TrimSuffix(p.Path, ".*")
					}

					if strings.HasSuffix(p.Path, "/*") {
						p.Path = strings.TrimSuffix(p.Path, "/*")
					}
				}
			}

			if *p.PathType == ingress.PathTypeImplementationSpecific {
				p.PathType = &defaultPathType
			}
		}
	}
}
