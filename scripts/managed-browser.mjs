import { createInterface } from "node:readline";
import { setTimeout as delay } from "node:timers/promises";

export function exposeManagedBrowser(page) {
  let stopped = false;
  let input = Promise.resolve();
  const lines = createInterface({ input: process.stdin, terminal: false });
  const keyPattern =
    /^(?:(?:Control|Shift|Alt)\+)?(?:Enter|Backspace|Delete|Tab|Escape|ArrowLeft|ArrowRight|ArrowUp|ArrowDown|Home|End|PageUp|PageDown|[a-z0-9])$/;
  lines.on("line", (line) => {
    if (line.length > 10000) return;
    let command;
    try {
      command = JSON.parse(line);
    } catch {
      return;
    }
    input = input
      .then(async () => {
        if (stopped || page.isClosed()) return;
        if (command.type === "click") {
          const size = await page.evaluate(() => ({
            width: innerWidth,
            height: innerHeight,
          }));
          if (
            Number.isFinite(command.x) &&
            Number.isFinite(command.y) &&
            command.x >= 0 &&
            command.y >= 0 &&
            command.x <= size.width &&
            command.y <= size.height
          )
            await page.mouse.click(command.x, command.y);
        } else if (command.type === "scroll") {
          await page.mouse.wheel(
            Math.max(-1500, Math.min(1500, Number(command.delta_x) || 0)),
            Math.max(-1500, Math.min(1500, Number(command.delta_y) || 0)),
          );
        } else if (
          command.type === "key" &&
          keyPattern.test(command.key || "")
        ) {
          await page.keyboard.press(command.key);
        } else if (
          command.type === "text" &&
          typeof command.text === "string" &&
          command.text.length <= 4096
        ) {
          await page.keyboard.insertText(command.text);
        } else if (command.type === "reload") {
          await page.reload({ waitUntil: "domcontentloaded" });
        }
      })
      .catch(() => {});
  });
  const frames = (async () => {
    while (!stopped && !page.isClosed()) {
      try {
        const dimensions = await page.evaluate(() => ({
          width: innerWidth,
          height: innerHeight,
        }));
        const jpeg = await page.screenshot({
          type: "jpeg",
          quality: 65,
          timeout: 5000,
        });
        if (!stopped)
          process.stdout.write(
            JSON.stringify({
              type: "frame",
              image: jpeg.toString("base64"),
              ...dimensions,
              path: new URL(page.url()).pathname,
            }) + "\n",
          );
      } catch {}
      await delay(1000, undefined, { ref: false });
    }
  })();
  return async () => {
    stopped = true;
    lines.close();
    await Promise.race([frames, delay(6000, undefined, { ref: false })]);
  };
}
