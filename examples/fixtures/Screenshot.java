// Screenshot.java - renders the fixture's "failure screenshot": a mock RCP
// dialog with the failing test and its assertion, watermarked SIMULATED so it
// can never be mistaken for a capture of a real product. Run as a single-file
// source program: java Screenshot.java out.png "title" "line" "line"...
import java.awt.*;
import java.awt.image.BufferedImage;
import java.io.File;
import javax.imageio.ImageIO;

public class Screenshot {
    public static void main(String[] args) throws Exception {
        String out = args[0], title = args[1];
        int w = 1024, h = 640;
        BufferedImage img = new BufferedImage(w, h, BufferedImage.TYPE_INT_RGB);
        Graphics2D g = img.createGraphics();
        g.setRenderingHint(RenderingHints.KEY_TEXT_ANTIALIASING, RenderingHints.VALUE_TEXT_ANTIALIAS_ON);
        // workbench background + a "shell"
        g.setColor(new Color(0xE8EAED)); g.fillRect(0, 0, w, h);
        int x = 96, y = 72, dw = 832, dh = 460;
        g.setColor(new Color(0x9AA0A6)); g.fillRoundRect(x + 6, y + 8, dw, dh, 10, 10);
        g.setColor(Color.WHITE); g.fillRoundRect(x, y, dw, dh, 10, 10);
        g.setColor(new Color(0x3C4043)); g.fillRoundRect(x, y, dw, 34, 10, 10); g.fillRect(x, y + 20, dw, 14);
        g.setColor(Color.WHITE); g.setFont(new Font(Font.SANS_SERIF, Font.BOLD, 15));
        g.drawString(title, x + 14, y + 23);
        // error banner
        g.setColor(new Color(0xFDECEA)); g.fillRect(x, y + 34, dw, 40);
        g.setColor(new Color(0xC5221F)); g.setFont(new Font(Font.SANS_SERIF, Font.BOLD, 14));
        g.drawString("✖ Test failed", x + 16, y + 59);
        // body lines
        g.setColor(new Color(0x202124)); g.setFont(new Font(Font.MONOSPACED, Font.PLAIN, 14));
        int ly = y + 104;
        for (int i = 2; i < args.length; i++) { g.drawString(args[i], x + 16, ly); ly += 22; }
        // fake widgets
        g.setColor(new Color(0xDADCE0)); g.drawRect(x + 16, y + dh - 120, dw - 32, 70);
        g.setColor(new Color(0x5F6368)); g.setFont(new Font(Font.SANS_SERIF, Font.PLAIN, 12));
        g.drawString("Repository: /tmp/egit.test/LocalRepositoriesTests1", x + 26, y + dh - 96);
        g.drawString("Branch: master    Staged: 3    Unstaged: 1", x + 26, y + dh - 74);
        for (int i = 0; i < 2; i++) {
            int bx = x + dw - 200 + i * 96;
            g.setColor(new Color(0xF1F3F4)); g.fillRoundRect(bx, y + dh - 40, 84, 26, 6, 6);
            g.setColor(new Color(0xDADCE0)); g.drawRoundRect(bx, y + dh - 40, 84, 26, 6, 6);
            g.setColor(new Color(0x202124)); g.drawString(i == 0 ? "Cancel" : "Finish", bx + 22, y + dh - 22);
        }
        // watermark
        g.setColor(new Color(255, 0, 0, 90)); g.setFont(new Font(Font.SANS_SERIF, Font.BOLD, 54));
        g.rotate(-0.35, w / 2.0, h / 2.0); g.drawString("SIMULATED FIXTURE", 250, 360);
        g.dispose();
        ImageIO.write(img, "png", new File(out));
    }
}
