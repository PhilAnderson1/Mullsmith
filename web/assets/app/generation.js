// Generation owns its in-flight operation and terminal transitions. The
// navigation port owns identity; the view port owns thread DOM assembly.
import { api, ApiError } from "/assets/app/api.js";
import { streamChat } from "/assets/app/sse.js";
import { toolCardNode, paintToolArgs, callFromEvent, paintCallOutcome, sharedFileFor, displayedImageFor } from "/assets/app/thread.js";
export function createGenerationController({
  navigation, view, getMeta, syncComposerButtons, toast, handleErr, applyTitle, showCtxFull, updateCtxMeter, promoteConversation
}) {
  const $ = (selector) => document.querySelector(selector);
  let inflight = null;
  const {
    openRound, cardHost, stopSpinner, syncRoundBody, syncAssistantVisibility, ensureThinking, artifactCard, displayImage, attachApproval, retireStrandedCards, settleRound, attachRegen, refreshLastAssistant, maybeScroll
  } = view;
  async function startGeneration(url, body, rollback, onAccepted, onUncertain) {
    const controller = new AbortController();
    const st = {
      controller,
      genId: "",
      msgId: "", // id of the round currently streaming (C7)
      started: false,
      errorData: null,
      nodes: null, // = st.cur.nodes: thinking/spinner helpers read this
      // Rounds in row order (C7): one entry per announced model round,
      // { id, nodes, raw, reasoning }. The bubble boundaries here ARE the
      // stored row boundaries — that is the whole live/reload agreement.
      rounds: [],
      byId: new Map(), // message_id → round, the target every event names
      cur: null, // the round currently streaming
      cards: {}, // call_id → { card, args } for the live tool cards
      // The ownership pair (review3 issue 2): WHICH conversation this
      // request addresses and WHICH view epoch owned the screen when it
      // started — the pair a navigation commit compares against before
      // disposing a generation the new thread replaces.
      convId: navigation.currentId,
      epoch: navigation.owner.current(),
    };
    inflight = st;
    syncComposerButtons();
    // One ownership rule for every live callback (finding 5, extended by
    // review3 issue 2): events belong on screen only while this is still
    // the live generation AND the conversation it addresses is the one on
    // display. The epoch is deliberately NOT part of live(): a repeat click
    // on the CURRENT conversation bumps navOwner without orphaning a stream
    // that still belongs there.
    const live = () => inflight === st && navigation.currentId === st.convId;
    // openRound opens one bubble per model round (C7). The `start` and
    // `round` events each name the row that round streams into, so a
    // tool-only round keeps its own (empty) prose and its own cards instead
    // of merging everything into one bubble stamped with the final row's id.
    // One terminal transition for the UNCERTAIN pre-start outcome (review3
    // issue 4): the request may have been accepted before the connection
    // died, so a rollback that ASSUMES refusal is not safe — the caller
    // reconciles against the server instead (reload, draft, toast).
    const settleUncertain = (err) => {
      if (inflight !== st) return;
      inflight = null;
      syncComposerButtons();
      if (onUncertain) onUncertain(err);
      else if (rollback) rollback();
    };
    try {
      await streamChat(
        url,
        body,
        {
          onStart(d) {
            if (!live()) return;
            navigation.recordActivity(); // a live stream IS newer activity (finding 4)
            st.started = true;
            st.genId = d.generation_id || "";
            st.msgId = d.message_id || "";
            if (onAccepted) onAccepted();
            openRound(st, st.msgId); // `start` names round 1's own row (C7)
            maybeScroll(true);
          },
          onRound(d) {
            if (!live()) return;
            // A fresh assistant row exists for the next model round (C7):
            // open the bubble it names, so the live split matches history.
            st.msgId = d.message_id || "";
            openRound(st, st.msgId);
            maybeScroll(true);
          },
          onToken(t) {
            if (!live()) return;
            stopSpinner(st); // output is flowing: the "waiting" dots are done
            if (!st.cur) return;
            st.cur.raw += t;
            st.cur.nodes.text.textContent = st.cur.raw; // plain text while streaming
            syncRoundBody(st.cur);
            maybeScroll();
          },
          onReasoning(t) {
            if (!live()) return;
            if (!st.cur) return;
            st.cur.reasoning += t;
            if (!getMeta() || !getMeta().show_reasoning || !st.nodes) return;
            ensureThinking(st);
            st.nodes.thinkPre.textContent = st.cur.reasoning;
            maybeScroll();
          },
          onToolStart(d) {
            if (!live()) return;
            if (!st.cur) return;
            const host = cardHost(st, d);
            if (!host) return;
            const quiet = getMeta()?.tool_cards === "useful" && Boolean(d.status_only);
            const card = quiet ? null : toolCardNode(d.call_id, d.tool);
            const args = d.arguments || "";
            if (card) {
              host.append(card);
              paintToolArgs(card, args);
            }
            st.cards[d.call_id] = { card, args, tool: d.tool };
            if (quiet) {
              const round = st.byId.get(d.message_id);
              if (round) syncAssistantVisibility(round.nodes.el);
            }
            maybeScroll();
          },
          onToolEnd(d) {
            if (!live()) return;
            const c = st.cards[d.call_id];
            if (!c) return;
            const round = st.byId.get(d.message_id);
            // The SAME projection a reload renders from tool_calls.display
            // (C7): one paintCallOutcome for both views.
            const view = callFromEvent(d);
            const quiet = getMeta()?.tool_cards === "useful" && view.statusOnly && view.status === "ok";
            if (!c.card && !quiet) {
              const host = cardHost(st, d);
              if (host) {
                c.card = toolCardNode(d.call_id, d.tool || c.tool);
                host.append(c.card);
                paintToolArgs(c.card, c.args);
              }
            }
            if (c.card) paintCallOutcome(c.card, view, getMeta()?.tool_cards);
            // files_share ⇒ a visible download card next to the card (§10.2).
            const file = sharedFileFor(view);
            if (file) {
              const host = cardHost(st, d);
              if (host) host.append(artifactCard(file));
            }
            const image = displayedImageFor(view);
            if (image) {
              const host = cardHost(st, d);
              const figure = displayImage(image);
              if (host && figure) host.append(figure);
            }
            if (round) syncAssistantVisibility(round.nodes.el);
            maybeScroll();
          },
          onAwaitingApproval(d) {
            if (!live()) return;
            if (!st.cur) return;
            const host = cardHost(st, d);
            if (!host) return;
            let c = st.cards[d.call_id];
            if (!c || !c.card) {
              const card = toolCardNode(d.call_id, d.tool);
              host.append(card);
              if (!c) c = st.cards[d.call_id] = { card, args: d.arguments || "", tool: d.tool };
              else c.card = card;
            }
            attachApproval(c.card, st, d);
            const round = st.byId.get(d.message_id);
            if (round) syncAssistantVisibility(round.nodes.el);
            maybeScroll(true);
          },
          onError(d) {
            st.errorData = d;
          },
          onTitle(d) {
            if (d.title) applyTitle(d.conversation_id || navigation.currentId, d.title);
          },
          onDone(d) {
            finishGeneration(st, d);
          },
        },
        controller.signal
      );
      // Upstream closed cleanly but never said done. After start the answer
      // was interrupted; BEFORE start the outcome is uncertain (review3
      // issue 4) — the request may have been accepted and the reply is
      // already durable — so reconcile exactly like a network death.
      if (inflight === st) {
        if (st.started) finishGeneration(st, { status: "lost" });
        else settleUncertain(new Error("The connection closed before the reply started."));
      }
    } catch (err) {
      if (err.name === "AbortError") {
        // One idempotent terminal transition (finding 5): finishGeneration
        // is the single owner of cleanup, and its own staleness check makes
        // it a no-op once the stream was abandoned. The old bare `return`
        // left inflight SET whenever Stop's abort API failed and the
        // fetch abort arrived here — the composer refused every later send.
        finishGeneration(st, { status: "aborted" });
        return;
      }
      if (err instanceof ApiError && err.status === 401) {
        // A late 401 from an ABANDONED request is not the current screen's
        // verdict (review2 finding 4): clearing inflight here erased a
        // newer live generation's bookkeeping. The generation that owns the
        // screen will meet its own auth failure if the session is really
        // gone.
        if (!live()) return;
        inflight = null;
        syncComposerButtons();
        handleErr(err, "send");
        return;
      }
      if (st.started) {
        finishGeneration(st, { status: "lost" });
      } else {
        // Pre-start rollback is screen-mutating (restores the draft, may
        // pin the ctx-full panel): an abandoned generation must not run it
        // against a composer that belongs to the newer one (review2
        // finding 4's reproduced sequence).
        if (!live()) return;
        // A DEFINITE refusal versus an uncertain loss (review3 issue 4): a
        // code in the envelope is this API's own rejection, written before
        // anything was accepted or persisted — rollback is honest. Without
        // a code the death came from transport or proxy, and whether the
        // POST landed is unknown: reconcile instead of hiding a durable
        // message behind a restored draft.
        if (err instanceof ApiError && err.code) {
          if (rollback) rollback();
          if (err.code === "context_full" || err.code === "history_limit") {
            // The thread can take no more turns (or no more MESSAGES — the
            // replay cap): pin the explanation where the fullness figures
            // live instead of a toast that fades in five seconds.
            showCtxFull(err.message);
          } else {
            toast(err.message || "Could not start a reply.");
          }
          inflight = null;
          syncComposerButtons();
        } else {
          settleUncertain(err);
        }
      }
    }
  }

  function finishGeneration(st, done) {
    stopSpinner(st); // every exit path must end the animation
    retireStrandedCards(st);
    if (inflight !== st) return; // abandoned mid-flight; bubble is gone
    inflight = null;
    navigation.recordActivity(); // the finished answer outranks any history on the wire
    syncComposerButtons();
    if (!st.rounds.length) return; // nothing ever streamed onto the screen
    const err = (st.errorData && st.errorData.message) || "";
    // Endpoint failures arrive after the assistant round has started, so
    // they historically appeared only as the round's small durable status
    // note. Announce them prominently too; the inline note remains after
    // the red toast fades and keeps the failed round meaningful on reload.
    if (st.errorData && st.errorData.code === "upstream_error" && err) toast(err);
    const outcomes = new Map((done.rounds || []).map((entry) => [entry.message_id, entry]));
    for (const r of st.rounds) {
      const entry = outcomes.get(r.id);
      if (entry) {
        settleRound(r, entry.status, entry.note,
          entry.completion_tokens, entry.duration_ms, entry.first_answer_ms,
          entry.generation_ms, err);
      } else {
        // Transport loss or a failed save can leave an announced round
        // without a durable outcome. Keep its identity and mark it interrupted.
        settleRound(r, done.status === "complete" ? "lost" : (done.status || "lost"),
          "", 0, 0, 0, 0, err);
      }
    }
    if (done.usage_tokens > 0) updateCtxMeter(done.usage_tokens); // freshest fullness figure
    for (const r of st.rounds) {
      // the button becomes visible now — make it work, per settled row
      if (r.nodes.el.dataset.id) {
        if (st.genId) r.nodes.el.dataset.gen = st.genId; // the shared turn marker (C7)
        attachRegen(r.nodes.el);
      }
    }
    refreshLastAssistant();
    maybeScroll();
  }

  async function stopStream() {
    const st = inflight;
    if (!st) return;
    $("#btn-stop").disabled = true;
    try {
      // Server-side abort persists the partial answer and closes with done.
      await api.abortGeneration(st.genId);
    } catch {
      st.controller.abort(); // endpoint gone anyway: just detach the stream
    } finally {
      $("#btn-stop").disabled = false;
    }
  }

  async function regenerateFrom(el, msgId) {
    // Same screen ownership as Send (review3 issue 2): while a navigation
    // is loading, this bubble may not even be on the thread that commits.
    if (inflight || navigation.navPendingId || navigation.reconcileId === navigation.currentId || !msgId) return;
    // Captured before the removal and the request: reloads and the reorder
    // belong to THIS thread, under THIS view epoch (review3 issues 2 and 4
    // meet here — a definite refusal and an uncertain death both reconcile
    // against the server, and neither may paint a screen we left).
    const convId = navigation.currentId;
    const view = navigation.owner.current();
    // Both terminal paths RELOAD, and both go through the recovery state:
    // while the truth is being re-fetched nothing else may address this
    // thread, an older snapshot can never paint over a newer answer, and a
    // failed reload is announced rather than swallowed (review3 finding 4).
    const reload = () => navigation.beginReconcile(convId, view);
    // Retire the WHOLE turn from the screen, not just the clicked bubble:
    // the server deletes every row of the generation, and the same marker
    // groups the streamed rounds.
    const gen = el.dataset.gen;
    for (const bubble of document.querySelectorAll(`#thread .msg[data-gen="${gen}"]`)) bubble.remove();
    await startGeneration(
      `/api/v1/messages/${encodeURIComponent(msgId)}/regenerate`,
      undefined,
      reload,
      () => promoteConversation(convId), // a fresh reply is a post too
      reload // uncertain acceptance: the deleted row may have been replaced — the server holds the truth
    );
  }

  function abandonInflight() {
    const st = inflight;
    if (!st) return;
    stopSpinner(st); // the bubble is going away; don't spin on a detached node
    inflight = null;
    st.controller.abort(); // kills the fetch; its AbortError lands on a dead token
    syncComposerButtons();
    if (st.genId) {
      api.abortGeneration(st.genId).catch(() => { /* already finished or gone */ });
    }
  }

  return {
    get current() { return inflight; },
    startGeneration, stopStream, regenerateFrom, abandonInflight,
    resolveApproval: (...args) => api.resolveApproval(...args),
  };
}
