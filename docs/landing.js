const links = Array.from(document.querySelectorAll(".stages a"));
const panels = links.map(link => document.getElementById(link.hash.slice(1)));
const play = document.getElementById("play");
const announcement = document.getElementById("stage-announcement");
let current = 0;
let timer = null;

function showStage(index, announce = false) {
  current = index;
  links.forEach((link, position) => {
    if (position === index) {
      link.setAttribute("aria-current", "step");
    } else {
      link.removeAttribute("aria-current");
    }
    panels[position].hidden = position !== index;
    panels[position].classList.toggle("is-active", position === index);
  });
  if (announce) {
    announcement.textContent = panels[index].querySelector("h3").textContent;
  }
}

function stopWalkthrough() {
  window.clearTimeout(timer);
  timer = null;
  play.textContent = "Play walkthrough";
}

function scheduleNextStage() {
  timer = window.setTimeout(() => {
    if (current === panels.length - 1) {
      stopWalkthrough();
      return;
    }
    showStage(current + 1, true);
    scheduleNextStage();
  }, 5500);
}

links.forEach((link, index) => {
  link.addEventListener("click", event => {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    stopWalkthrough();
    showStage(index, true);
  });
});

play.addEventListener("click", () => {
  if (timer !== null) {
    stopWalkthrough();
    return;
  }
  if (current === panels.length - 1) showStage(0, true);
  play.textContent = "Pause walkthrough";
  scheduleNextStage();
});

document.addEventListener("visibilitychange", () => {
  if (document.hidden && timer !== null) stopWalkthrough();
});

function stageFromHash() {
  const index = links.findIndex(link => link.hash === window.location.hash);
  if (index >= 0) {
    stopWalkthrough();
    showStage(index);
  }
}

window.addEventListener("hashchange", stageFromHash);
showStage(0);
stageFromHash();
play.hidden = false;
