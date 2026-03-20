REPO_ROOT := $(shell cd ../.. && pwd)

.PHONY: trace clean

trace: trace.jsonl

trace.jsonl:
	cd $(REPO_ROOT) && go run ./simulator/cmd/workloadgen \
		--jobs 60 --gpu-ratio 0.85 --amd-gpu-ratio 0.35 \
		--mean-inter-arrival-sec 0.08 --min-gpu-request 2 \
		--namespace demo --seed 42 --emit-workload-records=false \
		--out examples/10-kubectl-joulie-demo/trace.jsonl

clean:
	rm -f trace.jsonl kubeconfig.yaml
