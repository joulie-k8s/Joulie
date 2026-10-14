package workload

import (
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Profile is a trace job's workloadProfile as its pod's annotations carry
// it: utilizations and memory intensity in 0..1, written in percent with one
// decimal, and I/O intensity in 0..1, written with two.
type Profile struct {
	CPUUtil         float64
	GPUUtil         float64
	MemoryIntensity float64
	IOIntensity     float64
}

// Template is what every pod of a group shares. Requests holds cpu, memory
// and at most one extended GPU resource; Class is ClassPerformance or
// ClassStandard.
type Template struct {
	Namespace       string
	WorkloadID      string
	WorkloadType    string
	PodRole         string
	Class           string
	Gang            bool
	SubmitOffsetSec float64
	Requests        corev1.ResourceList
	Placement       Placement
	Profile         Profile
}

// JobSpec is one trace job: its jobId and its pod.
type JobSpec struct {
	JobID string
	Template
}

// Group is the trace jobs that become one Job object. JobIDs is in
// completion index order: the pod with batch.kubernetes.io/job-completion-index
// i runs JobIDs[i].
type Group struct {
	Key string
	Template
	JobIDs []string
}

// GroupJobs gathers jobs into groups by GroupKey, in the order the first job
// of each group appears, keeping the jobs' order inside each group. Jobs of
// one key whose pods differ (requests, class, placement, profile, namespace,
// gang) cannot share a pod template, so each distinct pod becomes a group of
// its own, keyed <key>#2, <key>#3 and so on; split lists, sorted, every key
// that was split this way, for one log line. A jobId written like a
// workload's key, or like one of its <key>#n, is never grouped with that
// workload: the group that comes later takes the next free #n, so every
// group's Key, and so its object name, is distinct. Job ids must be unique,
// which the loader checks.
func GroupJobs(jobs []JobSpec) (groups []Group, split []string) {
	type partKey struct {
		key  string
		solo bool
	}
	parts := map[partKey][]int{}
	used := map[string]bool{}
	splitSet := map[string]bool{}
	for _, j := range jobs {
		key := GroupKey(j.JobID, j.WorkloadID, j.PodRole, j.SubmitOffsetSec)
		pk := partKey{key, j.WorkloadID == ""}
		placed := false
		for _, gi := range parts[pk] {
			if apiequality.Semantic.DeepEqual(groups[gi].Template, j.Template) {
				groups[gi].JobIDs = append(groups[gi].JobIDs, j.JobID)
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		gkey, n := key, len(parts[pk])
		if n > 0 {
			gkey = key + "#" + strconv.Itoa(n+1)
			splitSet[key] = true
		}
		for i := n + 2; used[gkey]; i++ {
			gkey = key + "#" + strconv.Itoa(i)
		}
		used[gkey] = true
		parts[pk] = append(parts[pk], len(groups))
		groups = append(groups, Group{Key: gkey, Template: j.Template, JobIDs: []string{j.JobID}})
	}
	for k := range splitSet {
		split = append(split, k)
	}
	sort.Strings(split)
	return groups, split
}

// BuildOptions are the settings that shape a run's objects.
type BuildOptions struct {
	// RunID names the run; the objects carry RunLabelValue(RunID).
	RunID string
	// Intent is how pods declare their class; zero means DefaultIntent.
	Intent Intent
	// Image is the container image; empty means DefaultImage.
	Image string
	// Namespace replaces the trace's namespace when set; with neither, the
	// objects go to "default", as legacy pods do.
	Namespace string
	// NodeSelector and Tolerations are added to the trace's placement.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	// JobTTL is the Job objects' ttlSecondsAfterFinished, rounded down to
	// whole seconds; under one second leaves it unset, and the Job objects
	// stay until the run's cleanup deletes them.
	JobTTL time.Duration
}

func (o BuildOptions) intent() Intent {
	if o.Intent == "" {
		return DefaultIntent
	}
	return o.Intent
}

// BuildJob is the Job object of group g: completions and parallelism equal
// to the group's size, Indexed above one pod so the completion index maps a
// pod to its jobId, no retries, and the pod template of the group's jobs.
func BuildJob(g Group, o BuildOptions) *batchv1.Job {
	n := int32(len(g.JobIDs))
	mode := batchv1.NonIndexedCompletion
	if n > 1 {
		mode = batchv1.IndexedCompletion
	}
	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ObjectName(o.RunID, g.Key),
			Namespace: namespace(g.Template, o),
			Labels:    objectLabels(g.Template, o),
			Annotations: map[string]string{
				AnnotationRun:    o.RunID,
				AnnotationJobIDs: strings.Join(g.JobIDs, ","),
			},
		},
		Spec: batchv1.JobSpec{
			Parallelism:    &n,
			Completions:    &n,
			CompletionMode: &mode,
			BackoffLimit:   &backoff,
			Template:       podTemplate(g.Template, g.Key, o),
		},
	}
	if secs := int32(o.JobTTL / time.Second); secs > 0 {
		job.Spec.TTLSecondsAfterFinished = &secs
	}
	return job
}

// BuildPod is the bare pod of job j under OwnerPod: named by its jobId,
// carrying AnnotationJobID and, since there is no Job object to hold it,
// AnnotationRun, and otherwise the pod a Job object of j alone would create.
func BuildPod(j JobSpec, o BuildOptions) *corev1.Pod {
	tpl := podTemplate(j.Template, GroupKey(j.JobID, j.WorkloadID, j.PodRole, j.SubmitOffsetSec), o)
	tpl.Annotations[AnnotationJobID] = j.JobID
	tpl.Annotations[AnnotationRun] = o.RunID
	tpl.Name = ObjectName(o.RunID, j.JobID)
	tpl.Namespace = namespace(j.Template, o)
	return &corev1.Pod{ObjectMeta: tpl.ObjectMeta, Spec: tpl.Spec}
}

func namespace(t Template, o BuildOptions) string {
	switch {
	case o.Namespace != "":
		return o.Namespace
	case strings.TrimSpace(t.Namespace) != "":
		return t.Namespace
	}
	return "default"
}

// objectLabels are the labels of a run's Job objects and pods.
func objectLabels(t Template, o BuildOptions) map[string]string {
	l := map[string]string{
		LabelPartOf: PartOfWorkload,
		LabelRunID:  RunLabelValue(o.RunID),
	}
	if v := labelValue(t.WorkloadType); v != "" {
		l[LabelWorkloadType] = v
	}
	if v := labelValue(t.PodRole); v != "" {
		l[LabelPodRole] = v
	}
	return l
}

// podTemplate is the pod of every job of a group: the objects' labels; the
// group key, the profile and, under IntentAnnotation or IntentBoth, the class
// as annotations; one container with the requests, and GPU limits equal to
// the GPU requests, since an extended resource must be in limits; and the
// merged placement.
func podTemplate(t Template, groupKey string, o BuildOptions) corev1.PodTemplateSpec {
	ann := map[string]string{
		AnnotationGroup:             groupKey,
		AnnotationCPUUtilPct:        strconv.FormatFloat(t.Profile.CPUUtil*100, 'f', 1, 64),
		AnnotationGPUUtilPct:        strconv.FormatFloat(t.Profile.GPUUtil*100, 'f', 1, 64),
		AnnotationMemoryPressurePct: strconv.FormatFloat(t.Profile.MemoryIntensity*100, 'f', 1, 64),
		AnnotationIOIntensity:       strconv.FormatFloat(t.Profile.IOIntensity, 'f', 2, 64),
	}
	if in := o.intent(); in == IntentAnnotation || in == IntentBoth {
		ann[AnnotationWorkloadClass] = t.Class
	}
	res := corev1.ResourceRequirements{Requests: t.Requests.DeepCopy()}
	for name, q := range t.Requests {
		if isExtendedResource(name) {
			if res.Limits == nil {
				res.Limits = corev1.ResourceList{}
			}
			res.Limits[name] = q.DeepCopy()
		}
	}
	image := o.Image
	if image == "" {
		image = DefaultImage
	}
	p := mergePlacement(t.Placement, t.Class, o)
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(t, o), Annotations: ann},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: ContainerName, Image: image, Resources: res}},
			Affinity:      p.Affinity,
			NodeSelector:  p.NodeSelector,
			Tolerations:   p.Tolerations,
		},
	}
}

// isExtendedResource reports whether name is a vendor resource such as
// nvidia.com/gpu: qualified by a domain outside kubernetes.io.
func isExtendedResource(name corev1.ResourceName) bool {
	s := string(name)
	i := strings.Index(s, "/")
	if i < 0 {
		return false
	}
	domain := s[:i]
	return domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io")
}
