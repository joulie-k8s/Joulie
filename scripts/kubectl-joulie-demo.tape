# The kubectl joulie recording at the top of the README, played by VHS
# (https://github.com/charmbracelet/vhs) in scripts/kubectl-joulie-demo.sh,
# which mounts the plugin and the kubeconfig at /demo.
Output out/kubectl-joulie.gif
Set Shell bash
Set FontSize 14
Set Width 1500
Set Height 500
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Framerate 20

Hide
Type "export PATH=/demo/bin:$PATH KUBECONFIG=/demo/kubeconfig PS1='$ '; clear" Enter
Show

Type "kubectl joulie" Sleep 400ms Enter
Sleep 6s

Hide
Type "clear" Enter
Show
Type "kubectl joulie node mi300x-1" Sleep 400ms Enter
Sleep 6s

Hide
Type "clear" Enter
Show
Type "kubectl joulie top" Sleep 400ms Enter
# Let the sparklines fill: the twins change every controller manager
# reconcile, and a sparkline takes one point per change.
Hide
Sleep 60s
Show
Sleep 3s
Type "s" Sleep 2500ms
Down Sleep 800ms Down Sleep 1200ms
Enter Sleep 5s
Escape Sleep 1500ms
Type "p" Sleep 5s
Type "q" Sleep 1s
