// Navigation owns conversation identity, view epochs and recovery state.
import { api, ApiError } from "/assets/app/api.js";
import { LatestWins } from "/assets/app/ownership.js";
export function createNavigationController({
  view, getGeneration, abandon, renderConversation, syncComposerButtons, markActive, focusComposer, showAuth, toast, handleErr
}) {
  const state = {
    currentId: "", currentConv: null, navPendingId: "", reconcileId: "",
    threadRev: 0, focusOnOpen: false, lastCtxUsed: 0,
  };
  const navOwner = new LatestWins();
  const renderThread = (data) => view.renderThread(data);
  async function selectConversation(id) {
    // A repeated intent for the conversation a navigation is ALREADY loading
    // (C5): while its metadata or first page is in flight, currentId is not
    // yet id, so the committed check below cannot recognise the target — and
    // calling begin() here would orphan the one loader in flight (it loses
    // ownership, the early return starts no replacement, and the chosen
    // thread never renders). The pending navigation goes to this same place;
    // make sure the address names it and let that loader finish.
    if (id === state.navPendingId) {
      if (location.hash !== `#/c/${id}`) location.hash = `#/c/${id}`;
      return;
    }
    // Every other click is a navigation intent, including a repeat click on
    // the conversation already on screen (review2 finding 3): begin() runs
    // FIRST, so a still-pending request from an older click loses its
    // ownership. Returning before it let "select B, then click A again"
    // end with B's late response taking over the screen.
    const tok = navOwner.begin();
    // Any intent replaces or clears the pending marker (C5): it must never
    // outlive its loader, because ownership just moved and that loader will
    // not commit. The marker also drives Send's enablement (review3 issue
    // 2), so the buttons follow it in BOTH directions — set (a new
    // destination) and cleared (a repeat click on the visible thread).
    state.navPendingId = id === state.currentId ? "" : id;
    syncComposerButtons();
    if (id === state.currentId) return;
    // Ownership is captured BEFORE the first await (finding 1): with two
    // chats clicked in quick succession, only the youngest may commit. The
    // confirmed bug wrote currentConv/currentId from whichever response
    // landed LAST — the visible destination could differ from the user's
    // choice, and sends follow state.currentId, so this was more than a
    // flicker.
    await abandon(); // a live stream belongs to the old thread
    try {
      const conv = await api.getConversation(id);
      if (!navOwner.isCurrent(tok)) return; // superseded by a newer navigation
      // Metadata alone must not commit identity (C5): the FIRST PAGE rides
      // with it so conversation row, identity and rendered thread land
      // together. Committing on the metadata opened an interval where the
      // old thread stayed on screen while sends were already addressed to
      // id and the pickers pointed at its model.
      const page = await api.listMessages(id, "");
      if (!navOwner.isCurrent(tok)) return;
      // A generation this commit REPLACES must die with the old view
      // (review3 issue 2) — including work started after navigation began,
      // whose late `start`/token events would otherwise append into the
      // thread painted just now. Ownership is the pair (conversation, view
      // epoch): a generation belongs here only if both still match the
      // destination being committed.
      const gen = getGeneration();
      if (gen && (gen.convId !== id || gen.epoch !== tok)) abandon();
      state.currentConv = conv;
      state.currentId = id;
      state.navPendingId = "";
      if (state.reconcileId === id) state.reconcileId = ""; // this page IS the reconcile
      markActive();
      renderConversation();
      renderThread(page);
      if (state.focusOnOpen) {
        state.focusOnOpen = false;
        focusComposer();
      }
    } catch (err) {
      if (navOwner.isCurrent(tok)) state.navPendingId = ""; // ours to clear
      if (!navOwner.isCurrent(tok)) return; // a superseded request's failure is not this screen's
      // Failed navigation: the OLD thread is still the whole truth, so
      // enablement follows it back (review3 issue 2 — composer and committed
      // thread must never disagree).
      syncComposerButtons();
      if (err instanceof ApiError && err.status === 404) {
        toast("That conversation no longer exists.");
        state.currentId = "";
        location.hash = "";
        renderConversation();
        return;
      }
      // Nothing committed — the previous thread is still the whole truth,
      // so the address returns to what is on screen rather than stranding
      // a hash for a conversation that never rendered (C5).
      location.hash = state.currentId ? `#/c/${state.currentId}` : "";
      handleErr(err, "open conversation");
    }
  }

  async function loadFirstMessages(tok = navOwner.current(), convId = state.currentId) {
    // "Newer thread activity" is the activity counter, not only the epoch
    // (review3 finding 4): a second send does not bump the navigation owner,
    // yet the answer it produces must never be replaced by this snapshot.
    const rev = state.threadRev;
    const data = await api.listMessages(convId, "");
    if (!navOwner.isCurrent(tok) || state.currentId !== convId) return;
    if (state.threadRev !== rev) return;
    renderThread(data);
  }

  function beginReconcile(convId, view) {
    state.reconcileId = convId;
    syncComposerButtons();
    loadFirstMessages(view, convId).then(
      () => endReconcile(convId),
      (err) => {
        if (err instanceof ApiError && err.status === 401) showAuth();
        else if (state.reconcileId === convId) {
          toast("Could not reload the thread — refresh the page to check what was saved.", false);
        }
        endReconcile(convId);
      },
    );
  }

  function endReconcile(convId) {
    if (state.reconcileId !== convId) return; // a newer marker or commit owns this now
    state.reconcileId = "";
    syncComposerButtons();
  }

  function earlierButton(cursor) {
    const btn = view.earlierButtonNode();
    btn.addEventListener("click", async () => {
      const convId = state.currentId; // the page belongs to THIS thread
      // The view EPOCH, not just the ID (C5): leaving for B and returning to
      // A restores ID equality, but this page was requested against the
      // earlier visit — its rows may already be stale. Any navigation since
      // the click advances the token, and the page is dropped.
      const epoch = navOwner.current();
      try {
        const data = await api.listMessages(convId, cursor);
        if (state.currentId !== convId || !navOwner.isCurrent(epoch)) return; // navigated away mid-page
        const nextBtn = data.next_cursor ? earlierButton(data.next_cursor) : null;
        view.prependPage(btn,nextBtn,data.items);
      } catch (err) {
        handleErr(err, "load earlier messages");
      }
    }, { once: true });
    return btn;
  }

  return {
    get currentId() { return state.currentId; },
    get currentConv() { return state.currentConv; },
    get navPendingId() { return state.navPendingId; },
    get reconcileId() { return state.reconcileId; },
    get threadRev() { return state.threadRev; },
    get lastCtxUsed() { return state.lastCtxUsed; },
    owner: navOwner,
    selectConversation, loadFirstMessages, beginReconcile, earlierButton,
    reset(conv = null) {
      navOwner.bump();
      state.currentId = conv?.id || "";
      state.currentConv = conv;
      state.navPendingId = "";
      state.reconcileId = "";
      state.focusOnOpen = false;
      state.lastCtxUsed = 0;
      state.threadRev++;
    },
    updateConversation(conv) { state.currentConv = conv; },
    requestFocus() { state.focusOnOpen = true; },
    recordActivity() { state.threadRev++; },
    rememberUsage(used) { state.lastCtxUsed = used; },
  };
}
