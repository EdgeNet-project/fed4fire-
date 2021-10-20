package service

import (
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/EdgeNet-project/fed4fire/pkg/naming"

	"github.com/EdgeNet-project/fed4fire/pkg/utils"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/EdgeNet-project/fed4fire/pkg/identifiers"
	"github.com/EdgeNet-project/fed4fire/pkg/rspec"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

type AllocateOptions struct {
	// Optional. Requested expiration of all new slivers, may be ignored by aggregates.
	EndTime string `xml:"geni_end_time"`
}

type AllocateArgs struct {
	SliceURN    string
	Credentials []Credential
	Rspec       string
	Options     AllocateOptions
}

type AllocateReply struct {
	Data struct {
		Code  Code `xml:"code"`
		Value struct {
			Rspec   string   `xml:"geni_rspec"`
			Slivers []Sliver `xml:"geni_slivers"`
			Error   string   `xml:"geni_error"`
		} `xml:"value"`
	}
}

func (v *AllocateReply) SetAndLogError(err error, msg string, keysAndValues ...interface{}) error {
	klog.ErrorS(err, msg, keysAndValues...)
	v.Data.Code.Code = geniCodeError
	v.Data.Value.Error = fmt.Sprintf("%s: %s", msg, err)
	return nil
}

// Some things to take into account in request RSpecs:
// - Each node will have exactly one sliver_type in a request.
// - Each sliver_type will have zero or one disk_image elements.
//   If your testbed requires disk_image or does not support it,
//   it should handle bad requests RSpecs with the correct error.
// - The exclusive element is specified for each node in the request.
//   Your testbed should check if the specified value (in combination with the sliver_type) is supported,
//   and return the correct error if not.
// - The request RSpec might contain links that have a component_manager element that matches your AM.
//   If your AM does not support links, it should return the correct error.
// https://doc.fed4fire.eu/testbed_owner/rspec.html#request-rspec

// Some information will be in a request RSpec, that needs to be ignored and copied to the manifest RSpec unaltered.
// This is important to do correctly.
// - A request RSpec can contain nodes that have a component_manager_id set to a different AM.
//   You need to ignore these nodes, and copy them to the manifest RSpec unaltered.
// - A request RSpec can contain links that do not have a component_manager matching your AM
//   (links have multiple component_manager_id elements!).
//   You need to ignore these links, and copy them to the manifest RSpec unaltered.
// - A request RSpec can contain XML extensions in nodes, links, services, or directly in the rspec element.
//   Some of these the AM will not know.
//   It has to ignore these, and preferably also pass them unaltered to the manifest RSpec.
// https://doc.fed4fire.eu/testbed_owner/rspec.html#request-rspec

// Allocate allocates resources as described in a request RSpec argument to a slice with the named URN.
// On success, one or more slivers are allocated, containing resources satisfying the request, and assigned to the given slice.
// This method returns a listing and description of the resources reserved for the slice by this operation, in the form of a manifest RSpec.
// https://groups.geni.net/geni/wiki/GAPI_AM_API_V3#Allocate
func (s *Service) Allocate(r *http.Request, args *AllocateArgs, reply *AllocateReply) error {
	// Allocate moves 1 or more slivers from geni_unallocated (state 1) to geni_allocated (state 2).
	// This method can be described as creating an instance of the state machine for each sliver.
	// If the aggregate cannot fully satisfy the request, the whole request fails.
	// https://groups.geni.net/geni/wiki/GAPI_AM_API_V3/CommonConcepts#SliverAllocationStates
	userIdentifier, err := identifiers.Parse(r.Header.Get(utils.HttpHeaderUser))
	if err != nil {
		return reply.SetAndLogError(err, "Failed to parse user URN")
	}
	sliceIdentifier, err := identifiers.Parse(args.SliceURN)
	if err != nil {
		return reply.SetAndLogError(err, "Failed to parse slice URN")
	}
	credential, err := FindMatchingCredential(
		*userIdentifier,
		*sliceIdentifier,
		args.Credentials,
		s.TrustedCertificates,
	)
	if err == nil {
		klog.InfoS(
			"Found matching credential",
			"ownerUrn",
			credential.OwnerURN,
			"targetUrn",
			credential.TargetURN,
		)
	} else {
		return reply.SetAndLogError(err, "Invalid credentials")
	}

	// TODO: Implement RSpec passthroughs + node selection.
	requestRspec := rspec.Rspec{}
	err = xml.Unmarshal([]byte(html.UnescapeString(args.Rspec)), &requestRspec)
	if err != nil {
		return reply.SetAndLogError(err, "Failed to deserialize rspec")
	}

	// Build the sliver resources
	resources := make([]*sliverResources, len(requestRspec.Nodes))
	for i, node := range requestRspec.Nodes {
		resources[i], err = resourcesForRspec(
			node,
			s.AuthorityIdentifier,
			*sliceIdentifier,
			*userIdentifier,
			s.ContainerImages,
			s.ContainerCpuLimit,
			s.ContainerMemoryLimit,
		)
		if err != nil {
			return reply.SetAndLogError(err, "Failed to build resources")
		}
	}

	// Create the sliver resources and rollback them in case of failure
	success := true
	for _, res := range resources {
		_, err = s.ConfigMaps().Create(r.Context(), res.ConfigMap, metav1.CreateOptions{})
		if err == nil {
			klog.InfoS("Created configmap", "name", res.ConfigMap.Name)
		} else if !errors.IsAlreadyExists(err) {
			_ = reply.SetAndLogError(err, "Failed to create configmap", "name", res.ConfigMap.Name)
			success = false
		}
		_, err = s.Deployments().Create(r.Context(), res.Deployment, metav1.CreateOptions{})
		if err == nil {
			klog.InfoS("Created deployment", "name", res.Deployment.Name)
		} else if !errors.IsAlreadyExists(err) {
			_ = reply.SetAndLogError(err, "Failed to create deployment", "name", res.Deployment.Name)
			success = false
		}
		_, err = s.Services().Create(r.Context(), res.Service, metav1.CreateOptions{})
		if err == nil {
			klog.InfoS("Created service", "name", res.Service.Name)
		} else if !errors.IsAlreadyExists(err) {
			_ = reply.SetAndLogError(err, "Failed to create configmap", "name", res.Service.Name)
			success = false
		}
		if !success {
			break
		}
	}

	// Rollback in case of failure
	if !success {
		for _, res := range resources {
			err = s.ConfigMaps().Delete(r.Context(), res.ConfigMap.Name, metav1.DeleteOptions{})
			if err == nil {
				klog.InfoS("Deleted configmap", "name", res.ConfigMap.Name)
			} else {
				klog.InfoS("Failed to delete configmap", "name", res.ConfigMap.Name)
			}
			err = s.Deployments().Delete(r.Context(), res.Deployment.Name, metav1.DeleteOptions{})
			if err == nil {
				klog.InfoS("Deleted deployment", "name", res.Deployment.Name)
			} else {
				klog.InfoS("Failed to delete deployment", "name", res.Deployment.Name)
			}
			err = s.Services().Delete(r.Context(), res.Service.Name, metav1.DeleteOptions{})
			if err == nil {
				klog.InfoS("Deleted service", "name", res.Service.Name)
			} else {
				klog.InfoS("Failed to delete service", "name", res.Service.Name)
			}
		}
		return nil
	}

	returnRspec := rspec.Rspec{Type: rspec.RspecTypeRequest}
	for i, res := range resources {
		sliver := Sliver{
			URN:              res.Deployment.Annotations[fed4fireSliver],
			Expires:          res.Deployment.Annotations[fed4fireExpires],
			AllocationStatus: geniStateAllocated,
		}
		reply.Data.Value.Slivers = append(reply.Data.Value.Slivers, sliver)
		returnRspec.Nodes = append(returnRspec.Nodes, requestRspec.Nodes[i])
	}

	xml_, err := xml.Marshal(returnRspec)
	if err != nil {
		return reply.SetAndLogError(err, "Failed to serialize response")
	}
	reply.Data.Value.Rspec = string(xml_)
	reply.Data.Code.Code = geniCodeSuccess
	return nil
}

type sliverResources struct {
	ConfigMap  *corev1.ConfigMap
	Deployment *appsv1.Deployment
	Service    *corev1.Service
}

func deploymentImageForSliverType(
	sliverType rspec.SliverType,
	containerImages map[string]string,
) (string, error) {
	if sliverType.Name != "container" {
		return "", fmt.Errorf("invalid sliver type")
	}
	if len(sliverType.DiskImages) == 0 {
		return containerImages[utils.Keys(containerImages)[0]], nil
	}
	if len(sliverType.DiskImages) == 1 {
		identifier, err := identifiers.Parse(sliverType.DiskImages[0].Name)
		if err != nil {
			return "", err
		}
		if image, ok := containerImages[identifier.ResourceName]; ok {
			return image, nil
		} else {
			return "", fmt.Errorf("invalid image name")
		}
	}
	return "", fmt.Errorf("no more than one disk image can be specified")
}

func resourcesForRspec(
	node rspec.Node,
	authorityIdentifier identifiers.Identifier,
	sliceIdentifier identifiers.Identifier,
	userIdentifier identifiers.Identifier,
	containerImages map[string]string,
	cpuLimit string,
	memoryLimit string,
) (*sliverResources, error) {
	if node.Exclusive {
		return nil, fmt.Errorf("exclusive must be false")
	}
	if len(node.SliverTypes) != 1 {
		return nil, fmt.Errorf("exactly one sliver type must be specified")
	}
	image, err := deploymentImageForSliverType(node.SliverTypes[0], containerImages)
	if err != nil {
		return nil, err
	}
	sliceHash := naming.SliceHash(sliceIdentifier)
	sliverName, err := naming.SliverName(sliceIdentifier, node.ClientID)
	if err != nil {
		return nil, err
	}
	sliverIdentifier := authorityIdentifier.Copy(identifiers.ResourceTypeSliver, sliverName)
	annotations := map[string]string{
		fed4fireClientId: node.ClientID,
		// TODO: Create vacuum job.
		fed4fireExpires: (time.Now().Add(24 * time.Hour)).Format(time.RFC3339),
		fed4fireUser:    userIdentifier.URN(),
		fed4fireSlice:   sliceIdentifier.URN(),
		fed4fireSliver:  sliverIdentifier.URN(),
	}
	labels := map[string]string{
		fed4fireSliceHash:  sliceHash,
		fed4fireSliverName: sliverName,
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sliverName,
			Annotations: annotations,
			Labels:      labels,
		},
		Data: map[string]string{
			"authorized_keys": "",
		},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sliverName,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					fed4fireSliverName: sliverName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
					Labels:      labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  sliverName,
							Image: image,
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceCPU:    resource.MustParse(cpuLimit),
									corev1.ResourceMemory: resource.MustParse(memoryLimit),
								},
								Requests: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceCPU:    resource.MustParse(defaultCpuRequest),
									corev1.ResourceMemory: resource.MustParse(defaultMemoryRequest),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "ssh-volume",
									ReadOnly:  true,
									MountPath: "/root/.ssh/authorized_keys",
									SubPath:   "authorized_keys",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "ssh-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMap.Name,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sliverName,
			Annotations: annotations,
			Labels: map[string]string{
				fed4fireSliceHash:  sliceHash,
				fed4fireSliverName: sliverName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Port: 22}},
			Selector: map[string]string{
				fed4fireSliverName: sliverName,
			},
		},
	}

	return &sliverResources{configMap, deployment, service}, nil
}
