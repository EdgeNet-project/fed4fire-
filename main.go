// This package implements the Fed4Fire Aggregate Manager API for EdgeNet/Kubernetes.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"github.com/EdgeNet-project/edgenet/pkg/generated/clientset/versioned"
	"github.com/EdgeNet-project/fed4fire/pkg/identifiers"
	"github.com/EdgeNet-project/fed4fire/pkg/service"
	"github.com/EdgeNet-project/fed4fire/pkg/utils"
	"github.com/gorilla/rpc"
	"github.com/maxmouchet/gorilla-xmlrpc/xml"
	"io/ioutil"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"net/http"
	"os"
	"strings"
)

var showHelp bool
var authorityName string
var containerImages utils.ArrayFlags
var containerCpuLimit string
var containerMemoryLimit string
var namespaceCpuLimit string
var namespaceMemoryLimit string
var kubeconfigFile string
var insecure bool
var parentNamespace string
var serverAddr string
var serverCertFile string
var serverKeyFile string
var trustedRootCerts utils.ArrayFlags

func logRequest(i *rpc.RequestInfo) {
	klog.InfoS(
		"Received XML-RPC request",
		"proto", i.Request.Proto,
		"method", i.Request.Method,
		"uri", i.Request.RequestURI,
		"rpc-method", i.Method,
		"user-agent", i.Request.UserAgent(),
	)
}

func main() {
	klog.InitFlags(nil)
	flag.BoolVar(&showHelp, "help", false, "show this message")
	flag.StringVar(&authorityName, "authorityName", "example.org", "authority name to use in URNs")
	flag.Var(&containerImages, "containerImage", "name:image of a container image that can be deployed; can be specified multiple times")
	flag.StringVar(&containerCpuLimit, "containerCpuLimit", "2", "maximum amount of CPU that can be used by a container")
	flag.StringVar(&containerMemoryLimit, "containerMemoryLimit", "2Gi", "maximum amount of memory that can be used by a container")
	flag.StringVar(&namespaceCpuLimit, "namespaceCpuLimit", "8", "maximum amount of CPU that can be used by a slice subnamespace")
	flag.StringVar(&namespaceMemoryLimit, "namespaceMemoryLimit", "8Gi", "maximum amount of memory that can be used by a slice subnamespace")
	flag.StringVar(&kubeconfigFile, "kubeconfig", "", "path to the kubeconfig file used to communicate with the Kubernetes API")
	flag.BoolVar(&insecure, "insecure", false, "disable TLS client authentication")
	flag.StringVar(&parentNamespace, "parentNamespace", "", "kubernetes namespaces in which to create slice subnamespaces")
	flag.StringVar(&serverAddr, "serverAddr", "localhost:9443", "host:port on which to listen")
	flag.StringVar(&serverCertFile, "serverCert", "", "path to the server TLS certificate")
	flag.StringVar(&serverKeyFile, "serverKey", "", "path to the server TLS key")
	flag.Var(&trustedRootCerts, "trustedRootCert", "path to a trusted root certificate for authenticating user; can be specified multiple times")
	flag.Parse()

	if showHelp {
		flag.PrintDefaults()
		os.Exit(0)
	}

	caCertPool := x509.NewCertPool()
	for _, file := range trustedRootCerts {
		caCert, err := ioutil.ReadFile(file)
		utils.Check(err)
		caCertPool.AppendCertsFromPEM(caCert)
		klog.InfoS("Loaded trusted certificate", "file", file)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigFile)
	utils.Check(err)

	edgeclient, err := versioned.NewForConfig(config)
	utils.Check(err)

	kubeclient, err := kubernetes.NewForConfig(config)
	utils.Check(err)

	authorityIdentifier := identifiers.Identifier{
		Authorities:  []string{authorityName},
		ResourceType: identifiers.ResourceTypeAuthority,
		ResourceName: "am",
	}

	containerImages_ := make(map[string]string)
	for _, s := range containerImages {
		arr := strings.SplitN(s, ":", 2)
		containerImages_[arr[0]] = arr[1]
		klog.InfoS("Parsed container image name", "name", arr[0], "image", arr[1])
	}

	s := &service.Service{
		AbsoluteURL:          fmt.Sprintf("https://%s", serverAddr),
		AuthorityIdentifier:  authorityIdentifier,
		ContainerImages:      containerImages_,
		ContainerCpuLimit:    containerCpuLimit,
		ContainerMemoryLimit: containerMemoryLimit,
		NamespaceCpuLimit:    namespaceCpuLimit,
		NamespaceMemoryLimit: namespaceMemoryLimit,
		ParentNamespace:      parentNamespace,
		EdgenetClient:        edgeclient,
		KubernetesClient:     kubeclient,
	}

	xmlrpcCodec := xml.NewCodec()
	xmlrpcCodec.SetPrefix("Service.")

	RPC := rpc.NewServer()
	RPC.RegisterBeforeFunc(logRequest)
	RPC.RegisterCodec(xmlrpcCodec, "text/xml")
	utils.Check(RPC.RegisterService(s, ""))

	tlsConfig := &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	if insecure {
		tlsConfig.ClientAuth = tls.NoClientCert
		klog.InfoS("Disabled TLS client authentication")
	}

	server := &http.Server{
		Addr:      serverAddr,
		Handler:   RPC,
		TLSConfig: tlsConfig,
	}

	klog.InfoS("Listening", "address", serverAddr)
	utils.Check(server.ListenAndServeTLS(serverCertFile, serverKeyFile))
}
