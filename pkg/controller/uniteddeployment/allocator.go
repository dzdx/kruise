/*
Copyright 2019 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package uniteddeployment

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/klog"

	appsv1alpha1 "github.com/openkruise/kruise/pkg/apis/apps/v1alpha1"
)

type nameToReplicas struct {
	SubsetName string
	Replicas   int32
	Specified  bool
}

type subsetInfos []*nameToReplicas

func (n subsetInfos) Get(i int) *nameToReplicas {
	return []*nameToReplicas(n)[i]
}

func (n subsetInfos) Len() int {
	return len(n)
}

func (n subsetInfos) Less(i, j int) bool {
	if n[i].Replicas != n[j].Replicas {
		return n[i].Replicas < n[j].Replicas
	}

	return strings.Compare(n[i].SubsetName, n[j].SubsetName) < 0
}

func (n subsetInfos) Swap(i, j int) {
	n[i], n[j] = n[j], n[i]
}

// GetAllocatedReplicas returns a mapping from subset to next replicas.
// Next replicas is allocated by replicasAllocator, which will consider the current replicas of each subset and
// new replicas indicated from UnitedDeployment.Spec.Topology.Subsets.
func GetAllocatedReplicas(nameToSubset *map[string]*Subset, ud *appsv1alpha1.UnitedDeployment) (*map[string]int32, bool, string) {
	subsetInfos := getSubsetInfos(nameToSubset, ud)
	specifiedReplicas := getSpecifiedSubsetReplicas(ud)

	// call SortToAllocator to sort all subset by subset.Replicas in order of increment
	return subsetInfos.SortToAllocator().AllocateReplicas(*ud.Spec.Replicas, specifiedReplicas)
}

func (n subsetInfos) SortToAllocator() *replicasAllocator {
	sort.Sort(n)
	return &replicasAllocator{subsets: &n}
}

type replicasAllocator struct {
	subsets *subsetInfos
}

func (s *replicasAllocator) effectiveReplicas(replicas int32, subsetReplicasLimits *map[string]int32) (bool, string) {
	if subsetReplicasLimits == nil {
		return true, ""
	}

	var specifiedReplicas int32
	for _, replicas := range *subsetReplicasLimits {
		specifiedReplicas += replicas
	}

	if specifiedReplicas > replicas {
		return false, fmt.Sprintf("Specified subsets' replica (%d) is greater than UnitedDeployment replica (%d)", specifiedReplicas, replicas)
	} else if specifiedReplicas < replicas {
		specifiedCount := 0
		for _, subset := range *s.subsets {
			if _, exist := (*subsetReplicasLimits)[subset.SubsetName]; exist {
				specifiedCount++
			}
		}

		if specifiedCount == len(*s.subsets) {
			return false, fmt.Sprintf("Specified subsets' replica (%d) is less than UnitedDeployment replica (%d)", specifiedReplicas, replicas)
		}
	}

	return true, ""
}

func getSpecifiedSubsetReplicas(ud *appsv1alpha1.UnitedDeployment) *(map[string]int32) {
	replicaLimits := map[string]int32{}
	if ud.Spec.Topology.Subsets == nil {
		return &replicaLimits
	}

	for _, subsetDef := range ud.Spec.Topology.Subsets {
		if subsetDef.Replicas == nil {
			continue
		}

		if specifiedReplicas, err := ParseSubsetReplicas(*ud.Spec.Replicas, *subsetDef.Replicas); err == nil {
			replicaLimits[subsetDef.Name] = specifiedReplicas
		} else {
			klog.Warningf("Fail to consider the replicas of subset %s when parsing replicaLimits during managing replicas of UnitedDeployment %s/%s: %s",
				subsetDef.Name, ud.Namespace, ud.Name, err)
		}
	}

	return &replicaLimits
}

func getSubsetInfos(nameToSubset *map[string]*Subset, ud *appsv1alpha1.UnitedDeployment) *subsetInfos {
	infos := make(subsetInfos, len(ud.Spec.Topology.Subsets))
	for idx, subsetDef := range ud.Spec.Topology.Subsets {
		var replicas int32
		if subset, exist := (*nameToSubset)[subsetDef.Name]; exist {
			replicas = subset.Spec.Replicas
		}
		infos[idx] = &nameToReplicas{SubsetName: subsetDef.Name, Replicas: replicas}
	}

	return &infos
}

// AllocateReplicas will first try to check the specifiedSubsetReplicas is effective or not.
// If effective, normalAllocate will be called. It will apply these specified replicas, then average the rest replicas to left unspecified subsets.
// If not, it will incrementally allocate all of the replicas. The current replicas spread situation will be considered,
// in order to make the scaling smoothly
func (s *replicasAllocator) AllocateReplicas(replicas int32, specifiedSubsetReplicas *map[string]int32) (*map[string]int32, bool, string) {
	if effective, reason := s.effectiveReplicas(replicas, specifiedSubsetReplicas); !effective {
		return s.incrementalAllocate(replicas), false, reason
	}

	return s.normalAllocate(replicas, specifiedSubsetReplicas), true, ""
}

func (s *replicasAllocator) normalAllocate(expectedReplicas int32, specifiedSubsetReplicas *map[string]int32) *map[string]int32 {
	var specifiedReplicas int32
	specifiedSubsetCount := 0
	// Step 1: apply replicas to specified subsets, and mark them as specified = true.
	for _, subset := range *s.subsets {
		if replicas, exist := (*specifiedSubsetReplicas)[subset.SubsetName]; exist {
			specifiedReplicas += replicas
			subset.Replicas = replicas
			subset.Specified = true
			specifiedSubsetCount++
		}
	}

	// Step 2: averagely allocate the rest replicas to left unspecified subsets.
	leftSubsetCount := len(*s.subsets) - specifiedSubsetCount
	if leftSubsetCount != 0 {
		allocatableReplicas := expectedReplicas - specifiedReplicas
		average := int(allocatableReplicas) / leftSubsetCount
		remainder := int(allocatableReplicas) % leftSubsetCount

		for i := len(*s.subsets) - 1; i >= 0; i-- {
			subset := (*s.subsets)[i]
			if subset.Specified {
				continue
			}

			if remainder > 0 {
				subset.Replicas = int32(average + 1)
				remainder--
			} else {
				subset.Replicas = int32(average)
			}

			leftSubsetCount--

			if leftSubsetCount == 0 {
				break
			}
		}
	}

	return s.toSubsetReplicaMap()
}

func (s *replicasAllocator) incrementalAllocate(expectedReplicas int32) *map[string]int32 {
	var currentReplicas int32
	for _, nts := range *s.subsets {
		currentReplicas += nts.Replicas
	}

	consideredLen := len(*s.subsets)
	diff := expectedReplicas - currentReplicas

	var average int32
	var reminder int32
	var i int
	var leftSubsetsCount int32
	if diff > 0 {
		// UnitedDeployment is supposed to scale out replicas.
		// The policy here is try to allocate the new replicas as even as possible.
		// But this policy is also try not to affect the subset which has the replicas more than the average.
		// So it starts from the biggest index subset, which has the most replicas.
		for i = consideredLen - 1; i >= 0; i-- {
			// Consider the subsets from index 0 to i
			leftSubsetsCount = int32(i) + 1
			average = expectedReplicas / leftSubsetsCount
			consideredAverage := average
			reminder = expectedReplicas % leftSubsetsCount
			if reminder > 0 {
				consideredAverage++
			}
			// If the i th subset, which currently have the most replicas, has more replicas than the average, give up this try.
			if consideredAverage < s.subsets.Get(i).Replicas {
				expectedReplicas -= s.subsets.Get(i).Replicas
				continue
			}
			break
		}

		for j := i; j > -1; j-- {
			if reminder > 0 {
				s.subsets.Get(j).Replicas = average + 1
				reminder--
			} else {
				s.subsets.Get(j).Replicas = average
			}
		}

	} else if diff < 0 {
		// Right now, UnitedDeployment is scaling in.
		// It is also considering to allocate the replicas as average as possible. But this time, it is scaling in,
		// so the subsets which have the less replicas than the average replicas are not supposed to be bothered.
		for i = 0; i < consideredLen; i++ {
			leftSubsetsCount = int32(consideredLen - i)
			average = expectedReplicas / leftSubsetsCount
			reminder = expectedReplicas % leftSubsetsCount
			if average > s.subsets.Get(i).Replicas {
				expectedReplicas -= s.subsets.Get(i).Replicas
				continue
			}
			break
		}

		for j := i; j < consideredLen; j++ {
			if leftSubsetsCount <= reminder {
				s.subsets.Get(j).Replicas = average + 1
			} else {
				s.subsets.Get(j).Replicas = average
				leftSubsetsCount--
			}
		}
	}

	return s.toSubsetReplicaMap()
}

func (s *replicasAllocator) toSubsetReplicaMap() *map[string]int32 {
	allocatedReplicas := map[string]int32{}
	for _, subset := range *s.subsets {
		allocatedReplicas[subset.SubsetName] = subset.Replicas
	}

	return &allocatedReplicas
}

func (s *replicasAllocator) String() string {
	result := ""
	sort.Sort(s.subsets)
	for _, subset := range *s.subsets {
		result = fmt.Sprintf("%s %s -> %d;", result, subset.SubsetName, subset.Replicas)
	}

	return result
}
