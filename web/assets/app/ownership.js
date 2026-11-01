// Request ownership (findings 1 and 5): shared state may only be mutated by
// the YOUNGEST operation of a kind, and a stream's callbacks may only touch
// the screen while its generation is still the running one. The principle
// was already proven in loadFiles' sequence guard; this module makes it
// reusable — and testable.
//
// Pure and import-free on purpose (finding 8): the browser got these
// orderings wrong twice; the node suite (web/test/) drives out-of-order
// responses through it without a DOM.

export class LatestWins {
  #seq = 0;

  // begin() takes ownership for a new operation. Capture the token BEFORE
  // the first await — the capture point is what makes it meaningful.
  begin() {
    return ++this.#seq;
  }

  // current() joins the running sequence instead of taking it over: work
  // that extends the current operation (a "load more" page of the list
  // that is loading) belongs to it, not after it.
  current() {
    return this.#seq;
  }

  // isCurrent(tok) is the commit gate: false means a newer operation has
  // taken over and this result must be discarded, never committed.
  isCurrent(tok) {
    return tok === this.#seq;
  }

  // bump() retires every outstanding token without taking ownership: the
  // identity changed (sign-out, account deletion) and there is nothing
  // left for old responses to commit into.
  bump() {
    this.#seq++;
  }
}
