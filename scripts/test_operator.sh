#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
envtest_version=1.35.0
operator_image="managed-valkey/operator:${MV_RUN_ID:?MV_RUN_ID не задан}"
valkey_image=valkey/valkey:8.1.9
test_results_dir="${MV_STATE_DIR:?MV_STATE_DIR не задан}/test-results"

run_logged() {
    local name=$1 started_at status
    shift
    started_at=$(date +%s)
    set +e
    "$@" 2>&1 | tee "$test_results_dir/$name.log"
    status=${PIPESTATUS[0]}
    set -e
    printf '%s\t%s\t%s\n' "$name" "$started_at" "$(($(date +%s) - started_at))" \
        >>"$test_results_dir/durations.tsv"
    return "$status"
}

report_results() {
    local command_status=$? report_status

    trap - EXIT
    set +e
    "$repo_root/scripts/report_operator_test_matrix.sh" "$test_results_dir" 2>&1 |
        tee "$test_results_dir/matrix-report.log"
    report_status=${PIPESTATUS[0]}
    set -e
    if ((command_status != 0)); then
        exit "$command_status"
    fi
    exit "$report_status"
}

run_k3s_tests() {
    local name=$1
    local timeout=$2
    local pattern=$3
    run_logged "$name" env \
        MANAGED_VALKEY_ADMIN_KUBECONFIG="$ADMIN_KUBECONFIG" \
        MANAGED_VALKEY_OPERATOR_KUBECONFIG="${OPERATOR_KUBECONFIG:?OPERATOR_KUBECONFIG не задан}" \
        go test -v -tags=integration -count=1 "-timeout=$timeout" \
        -run "$pattern" ./operator/integration
}

cd "$repo_root"
mkdir -p "$test_results_dir"
: >"$test_results_dir/durations.tsv"
trap report_results EXIT
"$repo_root/scripts/check_operator_generated.sh"
run_logged unit go test -v -count=1 ./operator/... ./internal/...
run_logged envtest env \
    KUBEBUILDER_ASSETS="$(go tool setup-envtest use "$envtest_version" \
        --bin-dir "$repo_root/bin/envtest" -p path)" \
    go test -v -tags=envtest -count=1 -run '^TestEnvtest' ./operator/... ./internal/...

KUBECONFIG="${ADMIN_KUBECONFIG:?ADMIN_KUBECONFIG не задан}" \
    kubectl get --raw=/readyz >/dev/null
KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n valkey-system get \
    gateway/valkey clienttrafficpolicy/valkey-connections secret/valkey-wildcard-tls >/dev/null
run_logged environment-3 "$repo_root/scripts/check_operator_test_environment.sh" 3

docker build --file operator/Dockerfile --build-arg GO_BUILD_TAGS=integration --tag "$operator_image" .
docker pull "$valkey_image"
"$repo_root/scripts/load_k3s_image.sh" \
    "$MANAGED_VALKEY_COMPOSE_PROJECT" "$operator_image" \
    k3s-server k3s-agent-1 k3s-agent-2
"$repo_root/scripts/load_k3s_image.sh" \
    "$MANAGED_VALKEY_COMPOSE_PROJECT" "$valkey_image" \
    k3s-server k3s-agent-1 k3s-agent-2
export MANAGED_VALKEY_OPERATOR_IMAGE=$operator_image
export MANAGED_VALKEY_VALKEY_IMAGE=$valkey_image
export MANAGED_VALKEY_REPO_ROOT=$repo_root

run_k3s_tests k3s-three-failover 20m \
    '^(TestCT14HA03PendingReplicaBecomesRunningWhenCapacityReturns|TestHA05PDBRejectsEvictionWithTwoReadyPods|TestFP01FP02FP03SIGKILLValkeyProcesses|TestFP12MostAdvancedReplicaWinsFailover|TestHA08OP03OP04OP08FailoverStagesResumeAfterRestart|TestOP05CandidateLossBeforeAndAfterPromotion|TestOP06LiveUnknownCandidateBlocksSecondPromotion|TestRZ10FormerPrimaryWaitsForDirectReplicaUpstream|TestOP07TakeoverClosesDelayedAdministrativeConnection|TestFP04DeleteSinglePod)$'

run_k3s_tests k3s-three-partitions 22m \
    '^(TestNT06EnvoyAdminFailureDoesNotBlockObservationAndMutations|TestNT01NT02NT03AddressedNetworkPartitionsRecover|TestOP04ValkeyCommandRestartPointsAndLostResponse|TestNT04KubernetesOutageBlocksStaleActionsAndRecovers|TestNT05ReplicationPartitionKeepsEmptyReplacementOutOfFailover|TestFP06FP07OP08SIGSTOPSIGCONTProcesses|TestFP08FP09OP08BusyRecovery|TestFP10BothReplicasLostStayDegradedUntilReplacementSync|TestFP05FP11AllValkeyProcessesStopWhileOperatorIsDown|TestND05PrimaryLossWithoutSpareRecoversAfterCleanAgent)$'

run_k3s_tests k3s-three-lifecycle 18m \
    '^(TestSingleLifecycle|TestHA01HA04HA07FP04PW01RZ02RZ03Lifecycle|TestHA02HA04HA06InitialSafetyAndIsolation|TestPW01RZ01SinglePasswordAndResize|TestRZ03HAShrinkStartsEmpty)$'

run_k3s_tests k3s-three-rollout 27m \
    '^(TestRZ04PendingCapacityKeepsAcceptedRolloutUnapplied|TestRZ05PrimaryFailureKeepsRolloutTarget|TestRZ06UpdatedReplicaFailureWaitsForSynchronizedReplacement|TestRZ08HAShrinkRestartsKeepWorkloadStoppedUntilTargetTemplate|TestRZ08HARollingRestartsAtTemplateDeleteAndAppliedBoundaries|TestRZ09OldPodRestartKeepsOriginalConfig|TestRZ11EarlyGenerationAndDeletionKeepAcceptedRollout)$'

run_k3s_tests k3s-three-credentials-deletion 22m \
    '^(TestCT09CredentialLossRestoresWithoutRegenerationOrLeaks|TestCT10CT11SingleRegressionAfterHAFailureAndMutation|TestPW02PasswordHashAndSpecDeliveryOrder|TestPW03RotationStagesAndUnknownResultsResume|TestPW04PrimaryFailureDuringRotationUsesTargetPassword|TestPW05ReplicaFailureBeforeAndAfterConfirmation|TestPW06LiveIsolatedReplicaBlocksRotationUntilReturn|TestPW08FencedProcessKeepsAppOffDuringRotation|TestPW09CompletedRotationDoesNotDisconnectAndFuturePodUsesCurrentPassword|TestPW10PasswordPrecedesResizeAndDeletionPreemptsRotation|TestDL01DeletionDuringOperationsStopsEveryKnownProcess|TestDL04HADeletionRestartsAtEveryStage)$'

"$repo_root/scripts/test_environment.sh" expand-operator \
    "$MV_STATE_DIR" "$operator_image" "$valkey_image"
set -a
source "$MV_STATE_DIR/environment.env"
set +a
run_logged environment-4 "$repo_root/scripts/check_operator_test_environment.sh" 4
run_logged network-faults "$repo_root/scripts/check_operator_network_faults.sh"

run_k3s_tests k3s-four-nodes 22m \
    '^(TestCT06ManualFencingRestoresAfterRealAgentStop|TestOP01ClusterOperatorRecoversAfterSIGKILL|TestOP02LeaseLossStopsOperatorAndSuccessorResumes|TestND01WorkerLossFailsOverAndRestoresOnSpareNode|TestND02ReplicaWorkerLossUsesSpareNode|TestND03SingleWorkerLossStartsEmptyReplacement|TestND04OperatorAndPrimaryWorkerLossRecovers|TestND06WorkerLossWithEnvoyPreservesPublicIngress|TestND07ND08ND09NodeLossDuringProcessStopResumesAfterRestart)$'

run_k3s_tests k3s-four-mutations 14m \
    '^(TestRZ07AgentLossDuringRollingAndFullStopResumesRollout|TestPW07AgentLossCompletesRotationWithoutReplacementCapacity|TestDL02DL03NodeFailuresDuringDeletion)$'
