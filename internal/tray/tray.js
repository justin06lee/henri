// Draws henri's icon in the macOS menu bar. The daemon runs this as
//
//     osascript -l JavaScript tray.js <daemon-pid> <icon-path> <device-name> <henri-binary>
//
// and it takes itself down when that pid stops being its parent, so the icon
// is never in the menu bar while henri is not running -- which is the whole
// point of having one.
//
// Why a script and not AppKit from Go: a status item needs AppKit, AppKit
// needs cgo, and henri does not take cgo. macOS ships a bridge anyway --
// osascript's JavaScript-for-Automation can drive AppKit from a plain
// process -- so the icon costs a supervised child instead of a linker flag.
ObjC.import("AppKit");
ObjC.import("unistd");
ObjC.import("stdlib");

function run(argv) {
  const daemonPID = Number(argv[0]);
  const iconPath = argv[1];
  const deviceName = argv[2];
  const binary = argv[3];

  // Accessory: menu bar presence without a Dock icon or a menu of our own.
  const app = $.NSApplication.sharedApplication;
  app.setActivationPolicy($.NSApplicationActivationPolicyAccessory);

  ObjC.registerSubclass({
    name: "HenriTray",
    methods: {
      "tick:": {
        types: ["void", ["id"]],
        implementation: function () {
          // The daemon is this process's parent. When it dies the kernel
          // reparents us, getppid stops answering with its pid, and we leave.
          // There is no parent-death signal on macOS, so this poll is the
          // whole death watch; the daemon also kills us on a clean shutdown,
          // and this catches the unclean ones.
          if ($.getppid() !== daemonPID) $.exit(0);
        },
      },
      "sendNow:": {
        types: ["void", ["id"]],
        implementation: function () {
          const task = $.NSTask.alloc.init;
          task.executableURL = $.NSURL.fileURLWithPath(binary);
          task.arguments = $(["send"]);
          task.launchAndReturnError($());
        },
      },
    },
  });
  const target = $.HenriTray.alloc.init;

  const item = $.NSStatusBar.systemStatusBar.statusItemWithLength(
    $.NSVariableStatusItemLength
  );
  const img = $.NSImage.alloc.initWithContentsOfFile(iconPath);
  if (!img.isNil()) {
    img.size = $.NSMakeSize(18, 18);
    // A template image draws from its alpha channel and follows the menu
    // bar's light and dark modes; the icon file has no background for
    // exactly this reason.
    img.template = true;
    item.button.image = img;
  } else {
    item.button.title = "henri";
  }
  item.button.toolTip = "henri — shared clipboard";

  const menu = $.NSMenu.alloc.init;
  const info = $.NSMenuItem.alloc.initWithTitleActionKeyEquivalent(
    "henri — " + deviceName,
    null,
    ""
  );
  info.enabled = false;
  menu.addItem(info);
  menu.addItem($.NSMenuItem.separatorItem);
  const send = $.NSMenuItem.alloc.initWithTitleActionKeyEquivalent(
    "Send clipboard now",
    "sendNow:",
    ""
  );
  send.target = target;
  menu.addItem(send);
  item.menu = menu;

  $.NSTimer.scheduledTimerWithTimeIntervalTargetSelectorUserInfoRepeats(
    2,
    target,
    "tick:",
    null,
    true
  );
  $.NSRunLoop.currentRunLoop.run();
}
