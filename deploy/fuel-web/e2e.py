#!/usr/bin/env python3
"""Browser test of the Fuel web app against a TEST instance of fueld.

    FUEL_TOKEN=$(pass show agentd/fuel-token) ~/clawd/bin/pa-python deploy/fuel-web/e2e.py

It refuses any base URL that is not the fueld-e2e port, and it stops before
the first write unless the page shows the TEST banner (test_mode). It logs one
item through the chat, changes its amount, deletes it, and takes screenshots.

Browser: a Playwright Chromium when one is installed, else the Chrome on the
CDP port 9222 of this box, in a NEW isolated browser context (own cookies)
that is closed at the end. No other tab is touched.
"""
import os
import random
import re
import sys
import time

from playwright.sync_api import sync_playwright

BASE = os.environ.get("FUEL_WEB_URL", "http://100.120.65.8:8797")
TOKEN = os.environ.get("FUEL_TOKEN", "")
OUT = os.environ.get("FUEL_WEB_SHOTS", "/tmp")
CDP = os.environ.get("FUEL_WEB_CDP", "http://127.0.0.1:9222")

checks = []


def check(name, ok, detail=""):
    checks.append((name, bool(ok), detail))
    print(("PASS " if ok else "FAIL ") + name + (" :: " + str(detail) if detail and not ok else ""), flush=True)
    return ok


def main():
    if not re.search(r":8797$", BASE) or "8796" in BASE or "gojoe.run" in BASE:
        sys.exit("refused: the base URL is not the fueld-e2e instance")
    if not TOKEN:
        sys.exit("FUEL_TOKEN is empty")
    grams = random.randint(131, 169)
    food = random.choice(["cooked white rice", "boiled potatoes", "cooked lentils", "banana", "cooked pasta", "carrots"])
    text = "Log this as a new item (web e2e test): %d g %s" % (grams, food)
    with sync_playwright() as p:
        own = None
        try:
            own = p.chromium.launch()
            browser, how = own, "playwright chromium"
        except Exception:
            browser, how = p.chromium.connect_over_cdp(CDP), "chrome over CDP 9222, new isolated context"
        print("browser:", how, flush=True)
        ctx = browser.new_context(viewport={"width": 1440, "height": 900}, color_scheme="light")
        page = ctx.new_page()
        page.set_default_timeout(15000)
        errors, foreign, writes = [], [], []
        # Chrome ignores COOP on a plain http origin and says so; that is the test instance, not the page.
        page.on("console", lambda m: errors.append(m.text) if m.type == "error" and "Cross-Origin-Opener-Policy header has been ignored" not in m.text else None)
        page.on("pageerror", lambda e: errors.append("pageerror: " + str(e)))
        page.on("request", lambda r: (foreign.append(r.url) if not r.url.startswith(BASE + "/") and not r.url.startswith("blob:") else None,
                                      writes.append((r.method, r.url[len(BASE):])) if r.method != "GET" else None))
        try:
            run(page, ctx, text, grams, errors, foreign, writes)
        finally:
            ctx.close()
            if own:
                own.close()
    bad = [c for c in checks if not c[1]]
    print("SUMMARY: %d of %d checks passed (%s)" % (len(checks) - len(bad), len(checks), how))
    sys.exit(1 if bad else 0)


def until(fn, seconds):
    """Poll from Python: the page's CSP forbids the string evaluation that wait_for_function uses."""
    end = time.time() + seconds
    while time.time() < end:
        if fn():
            return True
        time.sleep(0.1)
    return False


def shot(page, name):
    path = os.path.join(OUT, "fuel-web-%s.png" % name)
    page.screenshot(path=path, timeout=20000)
    return path


def run(page, ctx, text, grams, errors, foreign, writes):
    resp = page.goto(BASE + "/fuel/web/")
    check("GET /fuel/web/ is 200", resp.status == 200, resp.status)
    csp = resp.headers.get("content-security-policy", "")
    check("the shell has a strict CSP", "default-src 'none'" in csp and "unsafe" not in csp, csp)
    page.wait_for_selector("form.login")
    check("the login page shows", page.locator("#token").is_visible())
    shot(page, "login-wide-light")

    # wrong token, then the right one
    page.fill("#token", "wrong-token")
    page.click("form.login button[type=submit]")
    page.locator("#login-msg", has_text="token").wait_for()
    check("a wrong token is refused", "Wrong token" in page.inner_text("#login-msg"))
    check("the token field is cleared", page.input_value("#token") == "")
    # The browser logs two expected 401 answers as console errors: the session probe of the first load and the wrong token.
    check("only the two expected 401 console lines before sign-in", len(errors) == 2 and all("401" in e for e in errors), errors)
    del errors[:]
    page.fill("#token", TOKEN)
    page.click("form.login button[type=submit]")
    page.wait_for_selector("#day-title")
    page.wait_for_selector("#day-table, #day-empty")
    check("logged in: the Day view shows", "(today)" in page.inner_text("#day-title"))
    # safety: the page must be the test instance before any write
    banner = page.locator("#test-banner")
    if not check("the TEST banner shows (test_mode)", banner.count() == 1 and "Fuel e2e" in banner.inner_text()):
        return
    cookies = ctx.cookies(BASE)
    check("the session cookie is HttpOnly and SameSite=Strict", len(cookies) == 1 and cookies[0]["httpOnly"] and cookies[0]["sameSite"] == "Strict", [c["name"] for c in cookies])
    check("the page stores nothing in the browser", page.evaluate("localStorage.length + sessionStorage.length") == 0)
    check("the budgets of the day show", page.locator(".tile").count() >= 5, page.locator(".tile").count())
    rows_before = page.locator("#day-table tbody tr").count()

    # the keyboard: one key focuses the composer
    page.locator("#day-title").click()
    page.keyboard.press("/")
    until(lambda: page.evaluate("document.activeElement && document.activeElement.id") == "composer", 3)
    check("the / key focuses the composer", page.evaluate("document.activeElement.id") == "composer")
    check("the / key types no slash", page.input_value("#composer") == "")

    # one text log through the chat
    page.keyboard.type(text)
    page.keyboard.press("Shift+Enter")
    page.keyboard.type("(second line)")
    check("Shift+Enter makes a new line", "\n" in page.input_value("#composer"))
    page.keyboard.press("Backspace")
    for _ in range(len("(second line)")):
        page.keyboard.press("Backspace")
    t0 = time.time()
    page.keyboard.press("Enter")
    page.wait_for_selector("#thinking")
    check("the thinking state shows", True)
    check("the composer is empty and keeps the focus", page.input_value("#composer") == "" and page.evaluate("document.activeElement.id") == "composer")
    check("Send is locked while the agent works", page.locator("#send").is_disabled())
    bubbles = page.locator(".msg.user", has_text=text)
    check("one bubble for the message while it runs", bubbles.count() == 1, bubbles.count())
    row = page.locator("#day-table tbody tr", has_text="%d g" % grams)
    row.first.wait_for(timeout=150000)
    page.wait_for_selector("#thinking", state="detached", timeout=150000)
    took = time.time() - t0
    print("agent turn: %.1f s" % took, flush=True)
    check("the new row is in the Day table", row.count() == 1 and page.locator("#day-table tbody tr").count() == rows_before + 1,
          (row.count(), rows_before, page.locator("#day-table tbody tr").count()))
    check("one bubble for the message after the answer", bubbles.count() == 1, bubbles.count())
    last = page.locator(".feed .msg").last
    check("the agent's answer and the log card show", "fuel" in (last.get_attribute("class") or "") and last.locator("ul.card li").count() >= 1 and len(last.locator(".reply p").first.inner_text()) > 0)
    check("exactly one POST /fuel/log", [w for w in writes if w[1] == "/fuel/log"] == [("POST", "/fuel/log")], writes)
    check("Send is free again", not page.locator("#send").is_disabled())
    shot(page, "day-wide-light")

    # edit the amount in the row
    key = row.get_attribute("data-row")
    row2 = page.locator("tr[data-row='%s']" % key)
    row2.locator(".amt-btn").click()
    field = row2.locator(".amt-input")
    field.wait_for()
    field.fill(str(grams + 20))
    field.press("Enter")
    page.locator("tr[data-row='%s'] .amt-btn" % key, has_text="%d g" % (grams + 20)).wait_for(timeout=30000)
    check("the amount edit shows in the row", "%d g" % (grams + 20) in row2.inner_text())
    check("exactly one POST /fuel/fix", len([w for w in writes if w[1] == "/fuel/fix"]) == 1, writes)
    # Esc cancels an edit and sends nothing
    row2.locator(".amt-btn").click()
    row2.locator(".amt-input").fill("999")
    row2.locator(".amt-input").press("Escape")
    page.wait_for_timeout(500)
    check("Esc cancels an edit", len([w for w in writes if w[1] == "/fuel/fix"]) == 1 and "%d g" % (grams + 20) in row2.inner_text())

    # delete with undo
    row2.locator("button[data-del]").click()
    undo = page.locator("button[data-undo='%s']" % key)
    undo.wait_for()
    undo.click()
    page.wait_for_timeout(5000)
    check("Undo keeps the row and sends nothing", page.locator("tr[data-row='%s'] button[data-del]" % key).count() == 1 and not [w for w in writes if w[1] == "/fuel/undo"], writes)
    page.locator("tr[data-row='%s'] button[data-del]" % key).click()
    page.locator("tr[data-row='%s']" % key).wait_for(state="detached", timeout=30000)
    check("the delete removes the row after the Undo window", page.locator("#day-table tbody tr").count() == rows_before or page.locator("#day-empty").count() == 1)
    check("exactly one POST /fuel/undo", len([w for w in writes if w[1] == "/fuel/undo"]) == 1, writes)
    page.reload()
    page.wait_for_selector("#day-table, #day-empty")
    check("after a reload the row is still gone and the session holds", page.locator("tr[data-row='%s']" % key).count() == 0)
    check("after a reload the feed has one bubble for the message", page.locator(".msg.user", has_text=text).count() == 1)

    # the day picker: yesterday and back
    page.click("#day-prev")
    until(lambda: "(today)" not in page.inner_text("#day-title"), 10)
    page.wait_for_selector("#day-table, #day-empty")
    check("the day picker opens the day before", re.search(r"#/day/\d{4}-\d{2}-\d{2}$", page.url) is not None, page.url)
    check("the picker is limited to 35 days", page.get_attribute("#day-pick", "min") is not None and page.get_attribute("#day-pick", "max") is not None)
    page.click("#day-next")
    page.locator("#day-title", has_text="(today)").wait_for()

    # Week and Dashboard
    page.click("a[data-nav=week]")
    page.wait_for_selector("#week-grid .card")
    check("Week shows the week budgets and the 7-day strip", page.locator("#week-grid .card").count() >= 5 and page.locator("#week-grid .card").first.locator(".cell").count() == 7,
          page.locator("#week-grid .card").count())
    page.click("button[data-tab=last7]")
    page.wait_for_selector("#week-grid .card")
    check("Week: last 7 days", page.locator("#week-grid .card").first.locator(".cell").count() == 7)
    page.click("button[data-tab=week]")
    page.click("a[data-nav=dashboard]")
    page.wait_for_selector("#dash-grid .card")
    dash = page.inner_text("#dash-grid")
    check("Dashboard shows budgets, levers, coffee, strength and calibration",
          all(w in dash for w in ["Budgets", "Levers", "Coffee by brew method", "Strength this week", "Maintenance calibration", "Provisional"]), dash[:300])
    links = page.eval_on_selector_all("#dash-grid a.info", "els => els.map(a => [a.href, a.target, a.rel])")
    check("the info links open the framework page in a new tab", len(links) >= 8 and all(h.startswith("https://") and "#" in h and t == "_blank" and "noopener" in r for h, t, r in links), links[:2])

    # screenshots: wide and phone, light and dark
    for scheme in ["light", "dark"]:
        page.emulate_media(color_scheme=scheme)
        page.set_viewport_size({"width": 1440, "height": 900})
        for view in ["day", "week", "dashboard"]:
            page.click("a[data-nav=%s]" % view)
            page.wait_for_selector({"day": "#day-table, #day-empty", "week": "#week-grid .card", "dashboard": "#dash-grid .card"}[view])
            page.wait_for_timeout(400)
            shot(page, "%s-wide-%s" % (view, scheme))
        page.set_viewport_size({"width": 390, "height": 844})
        for view in ["day", "chat", "week", "dashboard"]:
            page.click("a[data-nav=%s]" % view)
            page.wait_for_timeout(600)
            shot(page, "%s-phone-%s" % (view, scheme))
            if view == "day" and scheme == "light":
                over = page.evaluate("document.documentElement.scrollWidth - document.documentElement.clientWidth")
                check("nothing overflows at 390 px", over <= 0, over)
            if view == "chat" and scheme == "light":
                check("phone: Chat is its own tab", page.locator("#chat").is_visible() and not page.locator("#view").is_visible())
    page.emulate_media(color_scheme="light")
    page.set_viewport_size({"width": 1440, "height": 900})

    # sign out
    page.click("a[data-nav=day]")
    page.click("#logout")
    page.wait_for_selector("form.login")
    r = page.request.get(BASE + "/fuel/day")
    check("after Sign out the session is gone", r.status == 401, r.status)
    check("no console error and no CSP violation", not errors, errors[:5])
    check("no request to another origin", not foreign, foreign[:5])


if __name__ == "__main__":
    main()
