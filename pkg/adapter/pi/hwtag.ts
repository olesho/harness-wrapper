// harness-wrapper's tag extension for the Pi profile (pkg/adapter/pi): an
// input that starts with <!--hw:ID--> loses the tag, and the session gets a
// custom entry hw.input {id} before the input's user message, so a reader of
// the session file can tell which entry is which input. Its command,
// /hw-tag, shows a client (get_commands) that it loaded.
export default function (pi: any) {
	pi.registerCommand("hw-tag", {
		description: "harness-wrapper's input tags are on",
		handler: async () => {},
	});
	pi.on("input", async (event: any) => {
		const m = /^<!--hw:([A-Za-z0-9_.-]+)-->\n?/.exec(event.text);
		if (!m) return { action: "continue" };
		pi.appendEntry("hw.input", { id: m[1] });
		return { action: "transform", text: event.text.slice(m[0].length), images: event.images };
	});
}
