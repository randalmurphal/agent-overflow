package harnessrpc

import "testing"

func TestArmSoakSteadyStateSeedsOnceAndReusesActiveThread(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	config := SoakConfig{
		ScenarioName:      "soak-background-agents",
		ScenarioFileName:  "soak-scenario.json",
		ProjectName:       "soak-workspace",
		IdleThreadTitle:   "Soak: idle thread",
		ActiveThreadTitle: "Soak: background agents",
		Prompt:            "launch agents",
	}
	if err := ArmSoakSteadyState(receiver, config); err != nil {
		t.Fatalf("first ArmSoakSteadyState: %v", err)
	}
	threads, err := host.store.ListThreads()
	if err != nil || len(threads) != 2 {
		t.Fatalf("threads after first arm = %+v, %v; want two", threads, err)
	}
	if len(host.sent) != 1 {
		t.Fatalf("sent prompts after first arm = %v, want one", host.sent)
	}
	firstSend := host.sent[0]

	if err := ArmSoakSteadyState(receiver, config); err != nil {
		t.Fatalf("second ArmSoakSteadyState: %v", err)
	}
	threads, err = host.store.ListThreads()
	if err != nil || len(threads) != 2 {
		t.Fatalf("threads after second arm = %+v, %v; want the same two", threads, err)
	}
	if len(host.sent) != 2 || host.sent[1] != firstSend {
		t.Fatalf("second arm sends = %v, want the same active thread and prompt", host.sent)
	}
}
