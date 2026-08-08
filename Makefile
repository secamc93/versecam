# make install: compila e instala en ~/.local/bin (el versecam del PATH).
# El rm previo evita "Text file busy" si el panel esta corriendo; el proceso
# vivo no se afecta, solo hay que relanzarlo para tomar el binario nuevo.
BIN := $(HOME)/.local/bin/versecam

.PHONY: build install desktop desktop-install
build:
	go build -o versecam .

install: build
	rm -f $(BIN)
	cp versecam $(BIN)
	@echo "instalado: $(BIN) (relanza el panel para usarlo)"

# App de escritorio (Wails): requiere libgtk-3-dev y libwebkit2gtk-4.0-dev.
desktop:
	cd desktop && go build -tags desktop,production -o ../versecam-desktop .

desktop-install: desktop
	rm -f $(HOME)/.local/bin/versecam-desktop
	cp versecam-desktop $(HOME)/.local/bin/versecam-desktop
	mkdir -p $(HOME)/.local/share/icons/hicolor/scalable/apps $(HOME)/.local/share/icons/hicolor/256x256/apps $(HOME)/.local/share/applications
	cp assets/versecam.svg $(HOME)/.local/share/icons/hicolor/scalable/apps/versecam.svg
	# El PNG es para los docks/gestores que no leen SVG del tema de iconos.
	cp assets/versecam.png $(HOME)/.local/share/icons/hicolor/256x256/apps/versecam.png
	-gtk-update-icon-cache -f -t $(HOME)/.local/share/icons/hicolor 2>/dev/null
	printf '[Desktop Entry]\nType=Application\nName=Versecam\nComment=Panel de agentes de terminal (claude, codex, kimi, muse...) sobre tmux\nExec=%s/.local/bin/versecam-desktop\nIcon=versecam\nTerminal=false\nCategories=Development;Utility;\nStartupWMClass=versecam-desktop\n' "$(HOME)" > $(HOME)/.local/share/applications/versecam.desktop
	-update-desktop-database $(HOME)/.local/share/applications 2>/dev/null
	@echo "instalado: ~/.local/bin/versecam-desktop + lanzador de aplicaciones"
