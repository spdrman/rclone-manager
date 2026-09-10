// The attention note in an output block, and the one thing CSS could not
// do for it: stay open.
//
// Hover and focus already reveal it through :has(), and that alone made
// it useless for what it is for. The note carries a link, so a reader
// has to travel to it, and a hover-only bubble closes the moment the
// pointer leaves the term on its way there. So the first hover latches
// it open and only three things close it: the control in its corner, a
// click anywhere outside it, or Escape.
//
// Escape is not in the brief and is here anyway: WCAG 1.4.13 asks that
// content revealed on hover be dismissible without moving the pointer,
// and a keyboard user who has tabbed to the term has no other way out.
//
// No dependency on the CSS: with this file absent the hover rules still
// work, so the note degrades to non-sticky rather than to nothing.
(function () {
  "use strict";

  // Hand the CSS its cue that JavaScript is here, so it stops opening
  // this on :hover/:focus and leaves the class as the only opener. Two
  // owners meant the close control could not win: it returns focus to
  // the term, the CSS focus rule saw that, and the note reopened.
  document.documentElement.classList.add("tip-js");

  var blocks = document.querySelectorAll(".io");

  Array.prototype.forEach.call(blocks, function (io) {
    var term = io.querySelector(".tip-term");
    var bubble = io.querySelector(".tip-bubble");
    if (!term || !bubble) return;

    function open() {
      bubble.classList.add("is-open");
    }

    function close() {
      bubble.classList.remove("is-open");
    }

    term.addEventListener("mouseenter", open);
    term.addEventListener("focus", open);

    var closer = bubble.querySelector(".tip-close");
    if (closer) {
      closer.addEventListener("click", function (event) {
        event.preventDefault();
        // Focus first, close second, and the order is the whole trick.
        // focus() fires the listener above synchronously, which opens
        // it; closing afterwards leaves it shut with the term focused.
        // The other way round, the note reopened the moment it closed.
        //
        // Focus goes back to the term rather than nowhere because a
        // keyboard user would otherwise be dropped at the top of the
        // document when this button leaves the tab order.
        if (typeof term.focus === "function") term.focus();
        close();
      });
    }

    document.addEventListener("click", function (event) {
      if (!bubble.classList.contains("is-open")) return;
      // The term itself is a link. Clicking it navigates, so it is not
      // an "outside" click and must not be treated as one.
      if (bubble.contains(event.target) || term.contains(event.target)) return;
      close();
    });

    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape" || event.key === "Esc") close();
    });
  });
})();
