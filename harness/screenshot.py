"""Screenshot every dialler-admin page, light and dark, into /out.

Runs inside the Playwright container that harness/screenshot.sh starts on
the compose network; the UI is at https://admin:8443 with the harness
password. Pages are captured at desktop and phone widths.
"""
import os
import sys

from playwright.sync_api import sync_playwright

UI = os.environ.get("UI", "https://admin:8443")
PASSWORD = os.environ.get("PASSWORD", "harness")
OUT = "/out"
DEVICE = os.environ.get("DEVICE", "dev-a")

PAGES = [
    ("server", "/server"),
    ("clients", "/clients"),
    ("client", f"/clients/{DEVICE}"),
    ("client-new", "/clients/new"),
    ("calls", "/calls"),
    ("diagnostics", "/diagnostics"),
]


def shoot(page, name, theme, width):
    page.evaluate(f"document.documentElement.setAttribute('data-theme', '{theme}')")
    page.wait_for_timeout(150)
    page.screenshot(path=f"{OUT}/{name}-{theme}-{width}.png", full_page=True)


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch()
        for width in (1280, 430):
            ctx = browser.new_context(ignore_https_errors=True, viewport={"width": width, "height": 900}, device_scale_factor=1)
            page = ctx.new_page()
            page.goto(f"{UI}/login")
            page.wait_for_timeout(450)
            page.screenshot(path=f"{OUT}/login-light-{width}.png", full_page=True)
            page.fill("input[name=username]", "admin")
            page.fill("input[name=password]", PASSWORD)
            page.click("form button")
            page.wait_for_url("**/clients")
            for name, path in PAGES:
                page.goto(f"{UI}{path}")
                page.wait_for_load_state("networkidle")
                page.wait_for_timeout(450)  # let the page's fade-in finish
                for theme in ("light", "dark"):
                    shoot(page, name, theme, width)
            # The code page: add a device and capture the code and QR.
            page.goto(f"{UI}/clients/new")
            page.fill("input[name=user]", "290")
            page.fill("input[name=description]", "Screenshot test")
            page.click("form[action='/clients'] button")
            page.wait_for_selector(".bigcode")
            page.wait_for_timeout(450)
            for theme in ("light", "dark"):
                shoot(page, "enrol", theme, width)
            did = page.url.split("/clients/")[1].split("/")[0]
            page.goto(f"{UI}/clients/{did}")
            page.wait_for_selector("input[name=confirm]")
            page.fill("input[name=confirm]", did)
            page.click("form[action$='/purge'] button")
            page.wait_for_url("**/clients*")
            ctx.close()
        browser.close()
    print("ok", file=sys.stderr)


if __name__ == "__main__":
    main()
