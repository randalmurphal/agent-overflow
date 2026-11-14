package app

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/accountswitch"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/usermessage"
)

// continueLockTimeout bounds how long a continue after a usage limit waits
// for its thread's action lock.
const continueLockTimeout = 30 * time.Second

// accountSwitch returns the auto-switch coordinator
// (docs/specs/account-auto-switch.md). Triggers before
// startAccountSwitch only queue.
func (a *App) accountSwitch() *accountswitch.Coordinator {
	a.accountSwitcherOnce.Do(func() {
		var opts []accountswitch.Option
		if a.accountSwitchCheckInterval > 0 {
			opts = append(opts, accountswitch.WithCheckInterval(a.accountSwitchCheckInterval))
		}
		a.accountSwitcher = accountswitch.New(a.accountSwitchDeps(), opts...)
	})
	return a.accountSwitcher
}

// startAccountSwitch watches turns for usage-limit refusals and runs the
// coordinator's worker until shutdown.
func (a *App) startAccountSwitch() {
	a.accountSwitchStartOnce.Do(func() {
		a.subscribeGlobalTurnObserver(a.observeUsageLimitRefusal)
		a.accountSwitch().Start(a.lifeCtx())
	})
}

// observeUsageLimitRefusal reports a typed usage-limit refusal with the
// account the refused session was using, which for Codex can still be the
// previous selection. TurnRefused does not block, so it runs on the event
// worker.
func (a *App) observeUsageLimitRefusal(threadID string, evt provider.ProviderEvent) {
	if !provider.IsUsageLimitRefusal(evt) {
		return
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return
	}
	a.accountSwitch().TurnRefused(
		provider.UsageProviderFamily(sess.Provider), threadID,
		a.sessionRunningModel(threadID, sess), sess.CredentialAccountID,
	)
}

// accountSwitchBeforeSend gives the coordinator its send-path decision. The
// caller holds the thread's action lock and no selection lease.
func (a *App) accountSwitchBeforeSend(threadID, providerName, model string) {
	ownTurnInFlight := a.triage != nil && a.triage.HasInFlightTurnOrRound(threadID)
	a.accountSwitch().BeforeSend(provider.UsageProviderFamily(providerName), threadID, model, ownTurnInFlight)
}

func (a *App) accountSwitchDeps() accountswitch.Deps {
	return accountswitch.Deps{
		Now:      time.Now,
		Settings: a.accountSwitchConfig,
		Pool:     a.accountSwitchPool,
		Reading:  a.providerLifecycleService().Snapshot,
		RefreshReading: func(ctx context.Context, providerName, accountID string) error {
			return a.ensureProviderAccountManager().RefreshProviderAccountUsageContext(ctx, providerName, accountID)
		},
		RequestReading: a.providerLifecycleService().RequestReading,
		InFlight: func(providerName string) []accountswitch.ThreadModel {
			turns := a.providerTurnsInFlight(providerName)
			out := make([]accountswitch.ThreadModel, len(turns))
			for i, turn := range turns {
				out[i] = accountswitch.ThreadModel{ThreadID: turn.ThreadID, Model: turn.Model}
			}
			return out
		},
		Switch: func(providerName, accountID string) (bool, error) {
			account, err := a.switchProviderAccount(providerName, accountID, true)
			if provideraccountapp.IsNeedsLogin(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return account.SignInRequired, nil
		},
		// A Claude session rereads the shared credential on its next request
		// (applyProviderAccountSelectionToSessions). A Codex app-server keeps
		// its account until the reconnect at its next send.
		ReachesRunningTurns: func(providerName string) bool {
			return providerName == string(provider.Claude)
		},
		ContinueModel: a.threadContinueModel,
		Continue:      a.continueAfterUsageLimit,
		Report:        a.reportAccountAutoSwitch,
		ThreadNotice: func(threadID, text string) {
			if err := a.emitNoticeToThread(threadID, noticeKindAccountAutoSwitch, text); err != nil {
				log.Printf("account auto-switch: %v", err)
			}
		},
	}
}

// accountSwitchConfig maps the settings, which are validated on write and
// sanitized on load. The revision is read first, so a write that lands
// between the two revokes a wait admitted on these values.
func (a *App) accountSwitchConfig() accountswitch.Config {
	var revision uint64
	if a.settings != nil {
		revision = a.settings.Revision()
	}
	current := a.currentSettings()
	return accountswitch.Config{
		Revision:         revision,
		Strategy:         current.AccountAutoSwitchStrategy,
		ThresholdPercent: current.AccountAutoSwitchThreshold,
		AutoContinue:     current.AccountAutoContinue,
	}
}

func (a *App) accountSwitchPool(providerName string) accountswitch.Pool {
	if a.providerAccounts == nil {
		return accountswitch.Pool{}
	}
	listing := a.providerAccounts.AutoSwitchPool(providerName)
	pool := accountswitch.Pool{
		SelectedID: listing.SelectedID,
		Pinned:     !listing.AutoSelected,
		Accounts:   make([]accountswitch.Account, len(listing.Accounts)),
	}
	for i, account := range listing.Accounts {
		pool.Accounts[i] = accountswitch.Account{
			ID:     account.ID,
			Label:  account.Label,
			InPool: account.InPool,
			Usable: !account.LoginExpired,
		}
	}
	return pool
}

// threadContinueModel returns the model a continue of the thread would run
// on. ok is false for a thread that is gone, archived, or in a hidden mode
// (workflow-owned, scratch, holder).
func (a *App) threadContinueModel(threadID string) (model string, ok bool) {
	if a.store == nil {
		return "", false
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("account auto-switch: read thread %s: %v", threadID, err)
		return "", false
	}
	if thread.Archived || threadmode.IsHidden(thread.Mode) {
		return "", false
	}
	return a.threadRunningModel(thread), true
}

// continueAfterUsageLimit queues accountswitch.ContinueMessage to a thread a
// usage limit stopped when admit accepts it under the thread's action lock.
func (a *App) continueAfterUsageLimit(ctx context.Context, threadID string, admit func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, continueLockTimeout)
	defer cancel()
	return a.queueAgentNotice(ctx, agentNotice{
		threadID: threadID,
		sendID:   "usage-continue:" + threadID + ":" + uuid.NewString(),
		origin:   usermessage.OriginApp,
		prepare: func() (string, bool, error) {
			if !admit() {
				return "", false, nil
			}
			return accountswitch.ContinueMessage, true, nil
		},
		startFailed: func(err error) string {
			return "Could not start this thread to continue it after the usage limit: " + err.Error()
		},
	})
}

// reportAccountAutoSwitch publishes a switch. The Manager audits a switch
// it made; a failed one is audited here.
func (a *App) reportAccountAutoSwitch(report accountswitch.Report) {
	a.emit(eventchan.ProviderAccountAutoSwitch, report)
	if report.Error != "" && a.providerAccounts != nil {
		a.providerAccounts.AuditFailedAutoSwitch(report.Provider, report.FromAccountID, report.ToAccountID, report.Error)
	}
}
